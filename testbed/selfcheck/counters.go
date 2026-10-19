package selfcheck

import (
	"bufio"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Drops outside the testbed's impairments make a probe invalid: they would
// count as losses of the testbed.
//
//   - RcvbufErrors in the receiver's namespace: the probe read too slowly
//     and its socket buffer overflowed;
//   - the dropped column of /proc/net/softnet_stat, shared by all
//     namespaces: a CPU's packet backlog overflowed;
//   - drops of the bottleneck tbf: the probe overran the bottleneck;
//   - the drop columns of /proc/net/dev: a veth that receives through NAPI
//     (GRO on) drops packets when its ring is full, and counts them as
//     transmit drops of its peer.

// parseUDPRcvbufErrors reads RcvbufErrors from /proc/net/snmp, where a
// header line "Udp: InDatagrams NoPorts ..." precedes a line of values.
func parseUDPRcvbufErrors(snmp string) (uint64, error) {
	var header []string
	for line := range strings.Lines(snmp) {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "Udp:" {
			continue
		}
		if header == nil {
			header = fields
			continue
		}
		for i, name := range header {
			if name == "RcvbufErrors" && i < len(fields) {
				return strconv.ParseUint(fields[i], 10, 64)
			}
		}
		break
	}
	return 0, fmt.Errorf("no Udp RcvbufErrors in /proc/net/snmp")
}

// parseSoftnetDropped sums the second column of /proc/net/softnet_stat, one
// hexadecimal line per CPU.
func parseSoftnetDropped(stat string) (uint64, error) {
	var total uint64
	sc := bufio.NewScanner(strings.NewReader(stat))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			return 0, fmt.Errorf("softnet_stat line %q: want at least two columns", sc.Text())
		}
		v, err := strconv.ParseUint(fields[1], 16, 64)
		if err != nil {
			return 0, fmt.Errorf("softnet_stat: %w", err)
		}
		total += v
	}
	return total, sc.Err()
}

// parseDevDrops sums the receive and transmit drops of every interface but
// lo in /proc/net/dev, where two header lines precede one line per
// interface: "name: rx_bytes rx_packets rx_errs rx_drop ... tx_bytes
// tx_packets tx_errs tx_drop ...", eight receive and eight transmit columns.
func parseDevDrops(dev string) (uint64, error) {
	var total uint64
	for line := range strings.Lines(dev) {
		name, stats, ok := strings.Cut(line, ":")
		if !ok || strings.Contains(name, "|") || strings.TrimSpace(name) == "lo" {
			continue
		}
		fields := strings.Fields(stats)
		if len(fields) < 16 {
			return 0, fmt.Errorf("/proc/net/dev line %q: want 16 columns", line)
		}
		for _, i := range []int{3, 11} {
			v, err := strconv.ParseUint(fields[i], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("/proc/net/dev: %w", err)
			}
			total += v
		}
	}
	return total, nil
}

var tbfStats = regexp.MustCompile(`(?m)^qdisc tbf .*\n\s*Sent \d+ bytes \d+ pkt \(dropped (\d+),`)

// parseTbfDropped reads the drops of the tbf qdisc from "tc -s qdisc show";
// without a tbf it returns 0.
func parseTbfDropped(tc string) (uint64, error) {
	m := tbfStats.FindStringSubmatch(tc)
	if m == nil {
		if strings.Contains(tc, "qdisc tbf") {
			return 0, fmt.Errorf("no drop count of tbf in %q", tc)
		}
		return 0, nil
	}
	return strconv.ParseUint(m[1], 10, 64)
}
