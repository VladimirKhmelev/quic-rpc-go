//go:build linux

package env

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
)

// Collect records the environment of a run in the running testbed, built
// with the TCP setup tcp: the machine, the tools and the testbed.
func Collect(ctx context.Context, tcp netns.TCP) (*Env, error) {
	e := Machine("/")
	e.AddTools(ctx)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return e, e.AddTestbed(ctx, tcp)
}

// tools are the programs the testbed, the self-check and the benchmarks
// run, with the arguments that print their versions.
var tools = map[string][]string{
	"ip":      {"-V"},
	"tc":      {"-V"},
	"ethtool": {"--version"},
	"ping":    {"-V"},
	"iperf3":  {"--version"},
	"taskset": {"--version"},
}

// AddTools records the Go version and the first line each tool prints
// about its version, or "missing".
func (e *Env) AddTools(ctx context.Context) {
	e.Tools = map[string]string{"go": runtime.Version()}
	for name, args := range tools {
		cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed tools
		// Some tools translate the line.
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		out, err := cmd.CombinedOutput()
		line, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		if (err != nil && line == "") || strings.Contains(line, "not found") {
			line = "missing"
		}
		e.Tools[name] = line
	}
}

// tcpSysctls are the TCP settings recorded for the end hosts: those the
// testbed sets and those it leaves at the kernel's defaults.
var tcpSysctls = []string{
	"net.ipv4.tcp_congestion_control",
	"net.ipv4.tcp_no_metrics_save",
	"net.ipv4.tcp_slow_start_after_idle",
	"net.ipv4.tcp_rmem",
	"net.ipv4.tcp_wmem",
	"net.ipv4.tcp_sack",
	"net.ipv4.tcp_timestamps",
	"net.ipv4.tcp_ecn",
	"net.ipv4.tcp_autocorking",
}

// AddTestbed records the running testbed, built with the TCP setup tcp:
// the offloads of its veth ends, the qdiscs and routes of each namespace
// and the TCP settings of the end hosts.
func (e *Env) AddTestbed(ctx context.Context, tcp netns.TCP) error {
	tb := &Testbed{TCP: string(tcp), Namespaces: map[string]Namespace{}}
	devs := map[string][]string{}
	for _, l := range netns.Links {
		devs[l.A.NS] = append(devs[l.A.NS], l.A.Dev)
		devs[l.B.NS] = append(devs[l.B.NS], l.B.Dev)
	}
	lines := func(ns, name string, args ...string) ([]string, error) {
		out, err := netns.Command(ctx, ns, name, args...).Output()
		return strings.Split(strings.TrimSpace(string(out)), "\n"), err
	}
	for _, ns := range []string{netns.Client, netns.Router, netns.Server} {
		n := Namespace{Offloads: map[string]map[string]string{}}
		for _, dev := range devs[ns] {
			out, err := netns.Command(ctx, ns, "ethtool", "-k", dev).Output()
			if err != nil {
				return err
			}
			n.Offloads[dev] = parseOffloads(string(out))
		}
		var err error
		if n.Qdiscs, err = lines(ns, "tc", "qdisc", "show"); err != nil {
			return err
		}
		if n.Routes, err = lines(ns, "ip", "route", "show"); err != nil {
			return err
		}
		if ns != netns.Router {
			n.Sysctls = map[string]string{}
			for _, key := range tcpSysctls {
				out, err := netns.Command(ctx, ns, "sysctl", "-n", key).Output()
				if err != nil {
					return err
				}
				n.Sysctls[key] = strings.Join(strings.Fields(string(out)), " ")
			}
		}
		tb.Namespaces[ns] = n
	}
	e.Testbed = tb
	return nil
}

// offloadFeatures are the ethtool features the testbed sets, by the names
// "ethtool -k" prints.
var offloadFeatures = map[string]string{
	"tcp-segmentation-offload":     "tso",
	"generic-segmentation-offload": "gso",
	"generic-receive-offload":      "gro",
}

// parseOffloads reads the features the testbed sets from "ethtool -k".
func parseOffloads(out string) map[string]string {
	features := map[string]string{}
	for line := range strings.Lines(out) {
		name, state, ok := strings.Cut(line, ":")
		if short, known := offloadFeatures[strings.TrimSpace(name)]; ok && known {
			features[short], _, _ = strings.Cut(strings.TrimSpace(state), " ")
		}
	}
	return features
}
