package selfcheck

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const snmp = `Ip: Forwarding DefaultTTL InReceives
Ip: 2 64 100
Udp: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors InCsumErrors IgnoredMulti MemErrors
Udp: 11592531 69274 205129 3709547 205128 0 0 136 0
UdpLite: InDatagrams NoPorts InErrors OutDatagrams RcvbufErrors SndbufErrors InCsumErrors IgnoredMulti MemErrors
UdpLite: 0 0 0 0 7 0 0 0 0
`

func TestParseUDPRcvbufErrors(t *testing.T) {
	if got, err := parseUDPRcvbufErrors(snmp); err != nil || got != 205128 {
		t.Fatalf("got %d, %v; want 205128", got, err)
	}
	if _, err := parseUDPRcvbufErrors("Ip: Forwarding\nIp: 2\n"); err == nil {
		t.Fatal("no error without a Udp line")
	}
}

func TestParseSoftnetDropped(t *testing.T) {
	stat := "001da6a0 00000002 00000000 00000000\n001241a1 0000000a 00000000 00000001\n"
	if got, err := parseSoftnetDropped(stat); err != nil || got != 12 {
		t.Fatalf("got %d, %v; want 2+10", got, err)
	}
	if _, err := parseSoftnetDropped("001da6a0\n"); err == nil {
		t.Fatal("no error for a short line")
	}
}

func TestParseDevDrops(t *testing.T) {
	const dev = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 15849178115 4946574    0    9    0     0          0         0 15849178115 4946574    0    9    0     0       0          0
  eth0: 2202436726 1926194    0    2    0     0          0         0 163814063  653467    0   34    0     0       0          0
toserver: 100 1    0    0    0     0          0         0 200  2    0    5    0     0       0          0
`
	if got, err := parseDevDrops(dev); err != nil || got != 2+34+5 {
		t.Fatalf("got %d, %v; want the drops of every interface but lo, 41", got, err)
	}
	if _, err := parseDevDrops("  eth0: 1 2 3\n"); err == nil {
		t.Fatal("no error for a short line")
	}
}

func TestParseTbfDropped(t *testing.T) {
	const tc = `qdisc netem 1: root refcnt 13 limit 100000 delay 25ms  5ms loss 1% rate 100Gbit
 Sent 4142 bytes 37 pkt (dropped 5, overlimits 0 requeues 0)
 backlog 0b 0p requeues 0
qdisc tbf 10: parent 1:1 rate 10Mbit burst 3027b lat 198ms
 Sent 4142 bytes 37 pkt (dropped 3, overlimits 0 requeues 0)
 backlog 0b 0p requeues 0
`
	if got, err := parseTbfDropped(tc); err != nil || got != 3 {
		t.Fatalf("got %d, %v; want the 3 drops of tbf, not the 5 of netem", got, err)
	}
	if got, err := parseTbfDropped("qdisc netem 1: root\n Sent 0 bytes 0 pkt (dropped 9, overlimits 0 requeues 0)\n"); err != nil || got != 0 {
		t.Fatalf("without tbf: %d, %v", got, err)
	}
	if _, err := parseTbfDropped("qdisc tbf 10: parent 1:1 rate 10Mbit\n"); err == nil {
		t.Fatal("no error for a tbf without statistics")
	}
}

func TestParsePing(t *testing.T) {
	const out = `PING 10.0.2.2 (10.0.2.2) 56(84) bytes of data.
64 bytes from 10.0.2.2: icmp_seq=1 ttl=63 time=50.2 ms
64 bytes from 10.0.2.2: icmp_seq=3 ttl=63 time=49.80 ms

--- 10.0.2.2 ping statistics ---
3 packets transmitted, 2 received, 33.3333% packet loss, time 40ms
rtt min/avg/max/mdev = 49.800/50.000/50.200/0.200 ms
`
	got, err := parsePing(out)
	want := []time.Duration{50200 * time.Microsecond, 49800 * time.Microsecond}
	if err != nil || len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
}

func TestParseIperf(t *testing.T) {
	// Trimmed from a run on the impaired profile of check_test.go: the
	// receiver's totals are zero, the first second is short of the rate.
	const ok = `{"start": {}, "intervals": [], "end": {"sum_received": {"bits_per_second": 0}},
		"server_output_json": {"intervals": [
			{"sum": {"start": 0, "end": 1.000, "bits_per_second": 47500000}},
			{"sum": {"start": 1.000, "end": 2.000, "bits_per_second": 48540000}},
			{"sum": {"start": 2.000, "end": 3.000, "bits_per_second": 48530000}},
			{"sum": {"start": 3.000, "end": 4.000, "bits_per_second": 48550000}},
			{"sum": {"start": 4.000, "end": 5.000, "bits_per_second": 48540000}},
			{"sum": {"start": 5.000, "end": 5.039, "bits_per_second": 48620000}}],
		"end": {"sum": {"bytes": 0, "bits_per_second": 0}}}}`
	if got, n, err := parseIperf([]byte(ok)); err != nil || got != 48540000 || n != 4 {
		t.Fatalf("got %v over %d intervals, %v; want the median of the four whole intervals after the first second, 48540000", got, n, err)
	}
	refused := `{"start": {}, "intervals": [], "end": {}, "error": "unable to connect to server - server may not be running: Connection refused"}`
	if _, _, err := parseIperf([]byte(refused)); !errors.Is(err, errNotListening) {
		t.Fatalf("refused: %v, want errNotListening", err)
	}
	// The setup took two seconds: the empty intervals before the data and
	// the first one with data do not count.
	const late = `{"server_output_json": {"intervals": [
		{"sum": {"start": 0, "end": 1, "bits_per_second": 0}},
		{"sum": {"start": 1, "end": 2, "bits_per_second": 0}},
		{"sum": {"start": 2, "end": 3, "bits_per_second": 30000000}},
		{"sum": {"start": 3, "end": 4, "bits_per_second": 48540000}},
		{"sum": {"start": 4, "end": 5, "bits_per_second": 48530000}},
		{"sum": {"start": 5, "end": 6, "bits_per_second": 48550000}},
		{"sum": {"start": 6, "end": 7, "bits_per_second": 48540000}},
		{"sum": {"start": 7, "end": 7.04, "bits_per_second": 48000000}},
		{"sum": {"start": 7.04, "end": 8, "bits_per_second": 0}}]}}`
	if got, n, err := parseIperf([]byte(late)); err != nil || got != 48540000 || n != 4 {
		t.Fatalf("late start: %v over %d intervals, %v; want 48540000 over 4", got, n, err)
	}
	for name, out := range map[string]string{
		"no data": `{"server_output_json": {"intervals": [
			{"sum": {"start": 0, "end": 1, "bits_per_second": 0}},
			{"sum": {"start": 1, "end": 2, "bits_per_second": 0}},
			{"sum": {"start": 2, "end": 3, "bits_per_second": 0}},
			{"sum": {"start": 3, "end": 4, "bits_per_second": 0}},
			{"sum": {"start": 4, "end": 5, "bits_per_second": 0}}]}}`,
		"other error":      `{"end": {}, "error": "unable to read from stream socket: Resource temporarily unavailable"}`,
		"no server output": `{"end": {"sum_received": {"bits_per_second": 1}}}`,
		"too few intervals": `{"server_output_json": {"intervals": [
			{"sum": {"start": 0, "end": 1, "bits_per_second": 1}},
			{"sum": {"start": 1, "end": 2, "bits_per_second": 1}},
			{"sum": {"start": 2, "end": 2.5, "bits_per_second": 1}}]}}`,
		"not json": `iperf3: parameter error`,
	} {
		if _, _, err := parseIperf([]byte(out)); err == nil || errors.Is(err, errNotListening) {
			t.Errorf("%s: %v, want an error other than errNotListening", name, err)
		}
	}
}

func TestReport(t *testing.T) {
	r := &Report{Profile: "p"}
	r.add(Check{Name: "loss", Target: 1, Measured: 1.05})
	if !r.OK() || !r.Checks[0].OK {
		t.Fatalf("a check within tolerance failed: %+v", r)
	}
	r.add(Check{Name: "reorder", Target: 0, Measured: 0.001})
	if r.OK() {
		t.Fatal("a reordered packet against a target of zero passed")
	}
	valid := &Report{Profile: "p"}
	valid.add(Check{Name: "loss", Target: 1, Measured: 1})
	valid.invalid("socket buffer overflowed")
	if valid.OK() {
		t.Fatal("an invalid measurement passed")
	}
	for _, want := range []string{"INVALID MEASUREMENT", "socket buffer overflowed"} {
		if s := valid.String(); !strings.Contains(s, want) {
			t.Errorf("report misses %q:\n%s", want, s)
		}
	}
	if s := r.String(); !strings.Contains(s, "OUT OF TOLERANCE") || !strings.Contains(s, "FAIL") {
		t.Errorf("failed report:\n%s", s)
	}
}
