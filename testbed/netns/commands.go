package netns

import (
	"fmt"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Names of the namespaces.
const (
	Client = "client"
	Router = "router"
	Server = "server"
)

// Addresses of the client and the server. Traffic between them goes
// through the router.
var (
	ClientAddr = netip.MustParseAddr("10.0.1.2")
	ServerAddr = netip.MustParseAddr("10.0.2.2")
)

// End is one end of a veth pair.
type End struct {
	NS, Dev string
	Addr    netip.Prefix
	MAC     string
}

// Link is a veth pair.
type Link struct{ A, B End }

// Links is the topology: client — router — server. The impairments are
// applied in the router and only there (NET-1), see Paths.
var Links = []Link{
	{
		A: End{NS: Client, Dev: "eth0", Addr: netip.MustParsePrefix("10.0.1.2/24"), MAC: "02:00:00:00:01:02"},
		B: End{NS: Router, Dev: "toclient", Addr: netip.MustParsePrefix("10.0.1.1/24"), MAC: "02:00:00:00:01:01"},
	},
	{
		A: End{NS: Router, Dev: "toserver", Addr: netip.MustParsePrefix("10.0.2.1/24"), MAC: "02:00:00:00:02:01"},
		B: End{NS: Server, Dev: "eth0", Addr: netip.MustParsePrefix("10.0.2.2/24"), MAC: "02:00:00:00:02:02"},
	},
}

// Path is one direction through the router. Packets arrive on In and are
// redirected to the ifb device Ifb, whose netem delays, drops and reorders
// them; then they are routed out through Out, whose tbf is the bottleneck.
//
// netem and tbf sit on different devices because netem hands out the
// packets it reorders straight from its own queue, past any qdisc below
// it: under a tbf child they would skip the bottleneck and its queue.
type Path struct {
	Name, In, Ifb, Out string
}

// Paths are the two directions.
var Paths = []Path{
	{Name: "client→server", In: "toclient", Ifb: "fromclient", Out: "toserver"},
	{Name: "server→client", In: "toserver", Ifb: "fromserver", Out: "toclient"},
}

// Cmd is a command run in namespace NS, or outside the namespaces if NS is
// empty.
type Cmd struct {
	NS   string
	Args []string
}

func (c Cmd) String() string {
	s := strings.Join(c.Args, " ")
	if c.NS != "" {
		s = c.NS + ": " + s
	}
	return s
}

func in(ns string, args ...string) Cmd { return Cmd{NS: ns, Args: args} }

// TCP is the TCP setup of the client and the server (NET-6).
type TCP string

const (
	// TCPAligned brings Linux TCP close to quic-go: an initial window of 32
	// packets, pacing by fq and no slow start after an idle period. The
	// congestion control stays CUBIC.
	TCPAligned TCP = "aligned"
	// TCPDefault keeps the Linux defaults: an initial window of 10 packets,
	// no pacing and slow start after idle. It shows how much of a result
	// the defaults alone explain.
	TCPDefault TCP = "default"
)

// ParseTCP returns the TCP setup named s.
func ParseTCP(s string) (TCP, error) {
	switch t := TCP(s); t {
	case TCPAligned, TCPDefault:
		return t, nil
	}
	return "", fmt.Errorf("TCP setup %q: want %s or %s", s, TCPAligned, TCPDefault)
}

// tcpInitialWindow is the initial congestion and receive window of the
// aligned setup, as in quic-go (internal/congestion/cubic_sender.go).
const tcpInitialWindow = "32"

// UpCommands builds the topology with the TCP setup tcp and applies p.
func UpCommands(p Profile, tcp TCP) []Cmd {
	var cmds []Cmd
	for _, ns := range []string{Client, Router, Server} {
		cmds = append(cmds, Cmd{Args: []string{"ip", "netns", "add", ns}})
		forward := "0"
		if ns == Router {
			forward = "1"
		}
		cmds = append(cmds,
			in(ns, "sysctl", "-q", "-w",
				// No IPv6: no address checks after link up and no stray packets.
				"net.ipv6.conf.all.disable_ipv6=1", "net.ipv6.conf.default.disable_ipv6=1",
				// Set either way: a new namespace inherits it from the host.
				"net.ipv4.ip_forward="+forward),
			in(ns, "ip", "link", "set", "lo", "up"))
	}
	for _, l := range Links {
		// The explicit "name": ip takes a bare word that is a prefix of
		// one of its keywords, such as "a", for that keyword.
		cmds = append(cmds, Cmd{Args: []string{"ip", "link", "add",
			"name", l.A.Dev, "address", l.A.MAC, "netns", l.A.NS, "type", "veth",
			"peer", "name", l.B.Dev, "address", l.B.MAC, "netns", l.B.NS}})
		for _, e := range []struct{ self, peer End }{{l.A, l.B}, {l.B, l.A}} {
			cmds = append(cmds,
				in(e.self.NS, append([]string{"ethtool", "-K", e.self.Dev}, offloads(e.self.NS)...)...),
				in(e.self.NS, "ip", "addr", "add", e.self.Addr.String(), "dev", e.self.Dev),
				// Static neighbors: ARP would go through the impairments
				// and stall the first packets.
				in(e.self.NS, "ip", "neigh", "replace", e.peer.Addr.Addr().String(),
					"lladdr", e.peer.MAC, "dev", e.self.Dev, "nud", "permanent"),
				in(e.self.NS, "ip", "link", "set", e.self.Dev, "up"),
			)
		}
	}
	for _, host := range []struct {
		end End
		gw  netip.Addr
	}{{Links[0].A, Links[0].B.Addr.Addr()}, {Links[1].B, Links[1].A.Addr.Addr()}} {
		route := []string{"ip", "route", "add", "default", "via", host.gw.String()}
		if tcp == TCPAligned {
			route = append(route, "initcwnd", tcpInitialWindow, "initrwnd", tcpInitialWindow)
		}
		cmds = append(cmds, in(host.end.NS, route...))
		cmds = append(cmds, tcpCommands(host.end, tcp)...)
	}
	for _, path := range Paths {
		cmds = append(cmds,
			in(Router, "ip", "link", "add", "name", path.Ifb, "type", "ifb"),
			in(Router, "ip", "link", "set", path.Ifb, "up"),
			in(Router, "tc", "qdisc", "add", "dev", path.In, "handle", "ffff:", "ingress"),
			in(Router, "tc", "filter", "add", "dev", path.In, "parent", "ffff:",
				"matchall", "action", "mirred", "egress", "redirect", "dev", path.Ifb),
		)
	}
	return append(cmds, ApplyCommands(p)...)
}

// tcpCommands set up TCP on the end host e. The sysctls are set in both
// setups: a new namespace takes some of them from the host, and the
// testbed must not depend on the machine.
func tcpCommands(e End, tcp TCP) []Cmd {
	idle := "1"
	if tcp == TCPAligned {
		// quic-go keeps its window after an idle period; Linux starts over
		// from the initial window once the connection was idle for an RTO.
		idle = "0"
	}
	cmds := []Cmd{in(e.NS, "sysctl", "-q", "-w",
		"net.ipv4.tcp_congestion_control=cubic",
		// No metrics cache: a new connection would otherwise start from the
		// ssthresh and RTT of the last one to the same address, and runs
		// would depend on each other.
		"net.ipv4.tcp_no_metrics_save=1",
		"net.ipv4.tcp_rmem=4096 131072 33554432",
		"net.ipv4.tcp_wmem=4096 16384 4194304",
		"net.ipv4.tcp_slow_start_after_idle="+idle)}
	if tcp == TCPAligned {
		// Pacing, as quic-go does. The limits are high enough that fq never
		// drops a packet at the sender: a local loss would reach the TCP
		// stack and quic-go directly (NET-1). fq queues QUIC packets too.
		cmds = append(cmds, in(e.NS, "tc", "qdisc", "replace", "dev", e.Dev, "root",
			"fq", "limit", "100000", "flow_limit", "100000"))
	}
	return cmds
}

// offloads returns the ethtool features of an interface in namespace ns.
//
// TSO and GSO are off everywhere, and GRO on the router: with them a veth
// hands over packets of up to 64 KB, and netem would drop or delay each as
// one packet.
//
// GRO is on at the client and the server, after all impairments. It makes
// a veth receive through NAPI, a single ring that keeps packets in order.
// Without it a veth queues each packet on the backlog of the CPU that sent
// it, and the packets netem releases, some from its timer and some from new
// arrivals on another CPU, are taken in parallel: a plain 20 ms delay
// reordered about 0.2% of them.
func offloads(ns string) []string {
	gro := "on"
	if ns == Router {
		gro = "off"
	}
	return []string{"tso", "off", "gso", "off", "gro", gro}
}

// ApplyCommands set the impairments of p; on a running testbed they replace
// the old ones in place.
func ApplyCommands(p Profile) []Cmd {
	var cmds []Cmd
	for _, path := range Paths {
		cmds = append(cmds, qdiscCommands(path, p)...)
	}
	return cmds
}

// DownCommands remove the namespaces; the veth pairs go with them.
func DownCommands() []Cmd {
	var cmds []Cmd
	for _, ns := range []string{Server, Router, Client} {
		cmds = append(cmds, Cmd{Args: []string{"ip", "netns", "del", ns}})
	}
	return cmds
}

// netemLimit is the netem queue in packets. It holds every packet waiting
// for its delay, so it is far above the packets in flight of any profile;
// the bottleneck queue is the tbf on the way out.
const netemLimit = 100000

// netemRate is set together with jitter. With a rate, netem never sends a
// packet before the one queued ahead of it, so jitter does not reorder
// packets (NET-2); the rate itself is too high to limit anything.
const netemRate = "100gbit"

// qdiscCommands put netem for delay, jitter, loss and reordering on the ifb
// device of path and the bottleneck tbf, if the profile has a rate, on its
// way out; without a rate the way out has no queue, as a veth by default.
// Delayed packets wait in netem and do not take the bottleneck queue.
func qdiscCommands(path Path, p Profile) []Cmd {
	out := []string{"tc", "qdisc", "replace", "dev", path.Out, "root", "noqueue"}
	if p.Rate > 0 {
		out = []string{"tc", "qdisc", "replace", "dev", path.Out, "root", "handle", "10:",
			"tbf", "rate", p.Rate.String(), "burst", strconv.FormatUint(tbfBurst(p.Rate), 10), "limit", strconv.Itoa(p.QueueBytes())}
	}
	return []Cmd{netemCommand(path, p), in(Router, out...)}
}

// OutageCommands cut the link in both directions (on) or bring the
// impairments of p back. During an outage netem drops every new packet;
// the packets it holds still arrive, as those already on the wire.
func OutageCommands(p Profile, on bool) []Cmd {
	var cmds []Cmd
	for _, path := range Paths {
		if on {
			cmds = append(cmds, in(Router, "tc", "qdisc", "replace", "dev", path.Ifb, "root", "handle", "1:",
				"netem", "limit", strconv.Itoa(netemLimit), "loss", "100%"))
		} else {
			cmds = append(cmds, netemCommand(path, p))
		}
	}
	return cmds
}

func netemCommand(path Path, p Profile) Cmd {
	netem := []string{"tc", "qdisc", "replace", "dev", path.Ifb, "root", "handle", "1:",
		"netem", "limit", strconv.Itoa(netemLimit)}
	if oneWay := p.RTT / 2; oneWay > 0 {
		netem = append(netem, "delay", micros(oneWay))
		if p.Jitter > 0 {
			netem = append(netem, micros(p.Jitter), "distribution", "normal")
		}
	}
	switch {
	case p.Loss > 0 && p.LossBurst > 1:
		good2bad, bad2good := p.Gemodel()
		netem = append(netem, "loss", "gemodel", percent(good2bad), percent(bad2good))
	case p.Loss > 0:
		netem = append(netem, "loss", "random", percent(p.Loss))
	}
	if p.Reorder > 0 {
		netem = append(netem, "reorder", percent(p.Reorder))
	}
	if p.Jitter > 0 {
		netem = append(netem, "rate", netemRate)
	}
	return in(Router, netem...)
}

// tbfBurst is the token bucket size in bytes: a millisecond at the rate,
// and at least two full frames so that any packet fits.
func tbfBurst(r Rate) uint64 { return max(minQueueBytes, uint64(r)/8/1000) }

func micros(d time.Duration) string { return strconv.FormatInt(d.Microseconds(), 10) + "us" }

// percent formats p for tc with six decimals at most.
func percent(p Percent) string {
	return strconv.FormatFloat(math.Round(float64(p)*1e6)/1e6, 'f', -1, 64) + "%"
}
