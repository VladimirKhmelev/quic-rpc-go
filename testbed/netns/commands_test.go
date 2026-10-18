package netns

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

var degraded = Profile{
	Name: "degraded", RTT: 100 * time.Millisecond, Jitter: 20 * time.Millisecond,
	Loss: 2, LossBurst: 4, Reorder: 1, Rate: 10e6, QueueBDP: 1,
}

func TestApplyCommands(t *testing.T) {
	var want []Cmd
	for _, dev := range []string{"toserver", "toclient"} {
		want = append(want,
			Cmd{Router, strings.Fields("tc qdisc del dev " + dev + " root")},
			Cmd{Router, strings.Fields("tc qdisc add dev " + dev + " root handle 1: netem limit 100000" +
				" delay 50000us 20000us distribution normal loss gemodel 0.510204% 25% reorder 1% rate 100gbit")},
			Cmd{Router, strings.Fields("tc qdisc add dev " + dev + " parent 1:1 handle 10: tbf" +
				" rate 10000000bit burst 3028 limit 125000")},
		)
	}
	if got := ApplyCommands(degraded); !reflect.DeepEqual(got, want) {
		t.Fatalf("ApplyCommands:\n%s\nwant:\n%s", join(got), join(want))
	}
}

func TestQdiscCommands(t *testing.T) {
	tests := []struct {
		name string
		p    Profile
		want []string // the commands for toserver, without "tc qdisc add dev toserver"
	}{
		{"none", None, []string{"root handle 1: netem limit 100000"}},
		{"delay only", Profile{RTT: 5 * time.Millisecond}, []string{"root handle 1: netem limit 100000 delay 2500us"}},
		{"independent loss", Profile{RTT: time.Millisecond, Loss: 0.1, LossBurst: 1},
			[]string{"root handle 1: netem limit 100000 delay 500us loss random 0.1%"}},
		{"fast bottleneck", Profile{RTT: 150 * time.Millisecond, Rate: 100e6, QueueBDP: 1}, []string{
			"root handle 1: netem limit 100000 delay 75000us",
			"parent 1:1 handle 10: tbf rate 100000000bit burst 12500 limit 1875000",
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var want []Cmd
			for _, w := range tt.want {
				want = append(want, Cmd{Router, strings.Fields("tc qdisc add dev toserver " + w)})
			}
			if got := qdiscCommands("toserver", tt.p); !reflect.DeepEqual(got, want) {
				t.Fatalf("got:\n%s\nwant:\n%s", join(got), join(want))
			}
		})
	}
}

// TestTopology checks UpCommands against the link table: each end gets its
// own address, its peer as a static neighbor and no offloads, and only the
// router carries impairments.
func TestTopology(t *testing.T) {
	cmds := UpCommands(degraded)
	has := func(ns, args string) bool {
		return slices.ContainsFunc(cmds, func(c Cmd) bool { return c.NS == ns && strings.Join(c.Args, " ") == args })
	}
	for _, l := range Links {
		for _, e := range []struct{ self, peer End }{{l.A, l.B}, {l.B, l.A}} {
			for _, want := range []string{
				"ip addr add " + e.self.Addr.String() + " dev " + e.self.Dev,
				"ip neigh replace " + e.peer.Addr.Addr().String() + " lladdr " + e.peer.MAC + " dev " + e.self.Dev + " nud permanent",
				"ethtool -K " + e.self.Dev + " tso off gso off gro off",
			} {
				if !has(e.self.NS, want) {
					t.Errorf("no %q in %s", want, e.self.NS)
				}
			}
		}
		if l.A.Addr.Masked() != l.B.Addr.Masked() {
			t.Errorf("link %s.%s — %s.%s spans two subnets", l.A.NS, l.A.Dev, l.B.NS, l.B.Dev)
		}
	}
	for _, c := range cmds {
		if c.Args[0] == "tc" && c.NS != Router {
			t.Errorf("impairment outside the router: %v", c)
		}
	}
	// Up adds the same qdiscs as Apply, without removing old ones first.
	var qdiscs []Cmd
	for _, c := range ApplyCommands(degraded) {
		if !slices.Contains(c.Args, "del") {
			qdiscs = append(qdiscs, c)
		}
	}
	if tail := cmds[len(cmds)-len(qdiscs):]; !reflect.DeepEqual(tail, qdiscs) {
		t.Errorf("UpCommands ends with:\n%s\nwant the qdiscs of the profile:\n%s", join(tail), join(qdiscs))
	}
	for ns, forward := range map[string]string{Client: "0", Router: "1", Server: "0"} {
		if !has(ns, "sysctl -q -w net.ipv6.conf.all.disable_ipv6=1 net.ipv6.conf.default.disable_ipv6=1 net.ipv4.ip_forward="+forward) {
			t.Errorf("%s does not set ip_forward=%s", ns, forward)
		}
	}
}

func join(cmds []Cmd) string {
	var b strings.Builder
	for _, c := range cmds {
		b.WriteString("  " + c.String() + "\n")
	}
	return b.String()
}
