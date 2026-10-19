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
	for _, devs := range [][2]string{{"fromclient", "toserver"}, {"fromserver", "toclient"}} {
		want = append(want,
			Cmd{Router, strings.Fields("tc qdisc replace dev " + devs[0] + " root handle 1: netem limit 100000" +
				" delay 50000us 20000us distribution normal loss gemodel 0.510204% 25% reorder 1% rate 100gbit")},
			Cmd{Router, strings.Fields("tc qdisc replace dev " + devs[1] + " root handle 10: tbf" +
				" rate 10000000bit burst 3028 limit 125000")},
		)
	}
	if got := ApplyCommands(degraded); !reflect.DeepEqual(got, want) {
		t.Fatalf("ApplyCommands:\n%s\nwant:\n%s", join(got), join(want))
	}
}

func TestQdiscCommands(t *testing.T) {
	tests := []struct {
		name  string
		p     Profile
		netem string // the netem of fromclient, after "root handle 1: netem limit 100000"
		out   string // the root qdisc of toserver
	}{
		{"none", None, "", "noqueue"},
		{"delay only", Profile{RTT: 5 * time.Millisecond}, " delay 2500us", "noqueue"},
		{"independent loss", Profile{RTT: time.Millisecond, Loss: 0.1, LossBurst: 1}, " delay 500us loss random 0.1%", "noqueue"},
		{"fast bottleneck", Profile{RTT: 150 * time.Millisecond, Rate: 100e6, QueueBDP: 1},
			" delay 75000us", "handle 10: tbf rate 100000000bit burst 12500 limit 1875000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := []Cmd{
				{Router, strings.Fields("tc qdisc replace dev fromclient root handle 1: netem limit 100000" + tt.netem)},
				{Router, strings.Fields("tc qdisc replace dev toserver root " + tt.out)},
			}
			if got := qdiscCommands(Paths[0], tt.p); !reflect.DeepEqual(got, want) {
				t.Fatalf("got:\n%s\nwant:\n%s", join(got), join(want))
			}
		})
	}
}

// TestTopology checks UpCommands against the link table: each end gets its
// own address and its peer as a static neighbor, no segmentation offloads
// anywhere and GRO only at the ends; only the router carries impairments,
// each direction redirected to its ifb device.
func TestTopology(t *testing.T) {
	cmds := UpCommands(degraded, TCPAligned)
	has := func(ns, args string) bool {
		return slices.ContainsFunc(cmds, func(c Cmd) bool { return c.NS == ns && strings.Join(c.Args, " ") == args })
	}
	for _, l := range Links {
		for _, e := range []struct{ self, peer End }{{l.A, l.B}, {l.B, l.A}} {
			gro := map[bool]string{true: "off", false: "on"}[e.self.NS == Router]
			for _, want := range []string{
				"ip addr add " + e.self.Addr.String() + " dev " + e.self.Dev,
				"ip neigh replace " + e.peer.Addr.Addr().String() + " lladdr " + e.peer.MAC + " dev " + e.self.Dev + " nud permanent",
				"ethtool -K " + e.self.Dev + " tso off gso off gro " + gro,
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
	// fq on the end hosts paces TCP (NET-6); netem and tbf only in the router.
	for _, c := range cmds {
		if (slices.Contains(c.Args, "netem") || slices.Contains(c.Args, "tbf")) && c.NS != Router {
			t.Errorf("impairment outside the router: %v", c)
		}
	}
	for _, path := range Paths {
		for _, want := range []string{
			"ip link add name " + path.Ifb + " type ifb",
			"tc qdisc add dev " + path.In + " handle ffff: ingress",
			"tc filter add dev " + path.In + " parent ffff: matchall action mirred egress redirect dev " + path.Ifb,
		} {
			if !has(Router, want) {
				t.Errorf("no %q in the router", want)
			}
		}
	}
	if tail := cmds[len(cmds)-len(ApplyCommands(degraded)):]; !reflect.DeepEqual(tail, ApplyCommands(degraded)) {
		t.Errorf("UpCommands ends with:\n%s\nwant the qdiscs of the profile", join(tail))
	}
	for ns, forward := range map[string]string{Client: "0", Router: "1", Server: "0"} {
		if !has(ns, "sysctl -q -w net.ipv6.conf.all.disable_ipv6=1 net.ipv6.conf.default.disable_ipv6=1 net.ipv4.ip_forward="+forward) {
			t.Errorf("%s does not set ip_forward=%s", ns, forward)
		}
	}
}

func TestOutageCommands(t *testing.T) {
	var want []Cmd
	for _, ifb := range []string{"fromclient", "fromserver"} {
		want = append(want, Cmd{Router, strings.Fields("tc qdisc replace dev " + ifb + " root handle 1: netem limit 100000 loss 100%")})
	}
	if got := OutageCommands(degraded, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("outage:\n%s\nwant:\n%s", join(got), join(want))
	}
	// Back to the profile: its netem, and the bottleneck untouched.
	apply := ApplyCommands(degraded)
	if got := OutageCommands(degraded, false); !reflect.DeepEqual(got, []Cmd{apply[0], apply[2]}) {
		t.Fatalf("after the outage:\n%s", join(got))
	}
}

// TestTCPSetups checks the TCP commands of the end hosts in both setups.
func TestTCPSetups(t *testing.T) {
	tests := []struct {
		tcp         TCP
		route, idle string
		fq          bool
	}{
		{TCPAligned, " initcwnd 32 initrwnd 32", "0", true},
		{TCPDefault, "", "1", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.tcp), func(t *testing.T) {
			cmds := UpCommands(None, tt.tcp)
			has := func(ns, args string) bool {
				return slices.ContainsFunc(cmds, func(c Cmd) bool { return c.NS == ns && strings.Join(c.Args, " ") == args })
			}
			for _, host := range []struct{ ns, gw string }{{Client, "10.0.1.1"}, {Server, "10.0.2.1"}} {
				for _, want := range []string{
					"ip route add default via " + host.gw + tt.route,
					"sysctl -q -w net.ipv4.tcp_congestion_control=cubic net.ipv4.tcp_no_metrics_save=1" +
						" net.ipv4.tcp_rmem=4096 131072 33554432 net.ipv4.tcp_wmem=4096 16384 4194304" +
						" net.ipv4.tcp_slow_start_after_idle=" + tt.idle,
				} {
					if !has(host.ns, want) {
						t.Errorf("no %q in %s", want, host.ns)
					}
				}
				if fq := has(host.ns, "tc qdisc replace dev eth0 root fq limit 100000 flow_limit 100000"); fq != tt.fq {
					t.Errorf("%s: fq %v, want %v", host.ns, fq, tt.fq)
				}
			}
			for _, c := range cmds {
				if c.NS == Router && slices.Contains(c.Args, "fq") {
					t.Errorf("fq in the router: %v", c)
				}
			}
		})
	}
	if _, err := ParseTCP("tuned"); err == nil {
		t.Error("ParseTCP took an unknown setup")
	}
}

func join(cmds []Cmd) string {
	var b strings.Builder
	for _, c := range cmds {
		b.WriteString("  " + c.String() + "\n")
	}
	return b.String()
}
