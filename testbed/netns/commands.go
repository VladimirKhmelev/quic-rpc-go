package netns

import (
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

// Links is the topology: client — router — server. Direction client →
// server leaves the router through toserver, the opposite one through
// toclient; the impairments are applied there and only there (NET-1).
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

// impairedDevs are the router interfaces that carry the impairments.
var impairedDevs = []string{"toserver", "toclient"}

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

// UpCommands builds the topology and applies p.
func UpCommands(p Profile) []Cmd {
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
				// Offloads off: with them a veth hands over packets of up
				// to 64 KB, and netem drops or delays each as one packet.
				in(e.self.NS, "ethtool", "-K", e.self.Dev, "tso", "off", "gso", "off", "gro", "off"),
				in(e.self.NS, "ip", "addr", "add", e.self.Addr.String(), "dev", e.self.Dev),
				// Static neighbors: ARP would go through the impairments
				// and stall the first packets.
				in(e.self.NS, "ip", "neigh", "replace", e.peer.Addr.Addr().String(),
					"lladdr", e.peer.MAC, "dev", e.self.Dev, "nud", "permanent"),
				in(e.self.NS, "ip", "link", "set", e.self.Dev, "up"),
			)
		}
	}
	cmds = append(cmds,
		in(Client, "ip", "route", "add", "default", "via", Links[0].B.Addr.Addr().String()),
		in(Server, "ip", "route", "add", "default", "via", Links[1].A.Addr.Addr().String()),
	)
	for _, dev := range impairedDevs {
		cmds = append(cmds, qdiscCommands(dev, p)...)
	}
	return cmds
}

// ApplyCommands replace the impairments of the running testbed with p.
// Packets pass unimpaired for the moment between removing the old qdiscs
// and adding the new ones.
func ApplyCommands(p Profile) []Cmd {
	var cmds []Cmd
	for _, dev := range impairedDevs {
		cmds = append(cmds, in(Router, "tc", "qdisc", "del", "dev", dev, "root"))
		cmds = append(cmds, qdiscCommands(dev, p)...)
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
// the bottleneck queue is the tbf below it.
const netemLimit = 100000

// netemRate is set together with jitter. With a rate, netem never sends a
// packet before the one queued ahead of it, so jitter does not reorder
// packets (NET-2); the rate itself is too high to limit anything.
const netemRate = "100gbit"

// qdiscCommands put netem for delay, jitter, loss and reordering at the
// root of dev and, if the profile has a rate, tbf below it for the
// bottleneck: delayed packets wait in netem and do not take the bottleneck
// queue.
func qdiscCommands(dev string, p Profile) []Cmd {
	netem := []string{"tc", "qdisc", "add", "dev", dev, "root", "handle", "1:",
		"netem", "limit", strconv.Itoa(netemLimit)}
	if oneWay := p.RTT / 2; oneWay > 0 {
		netem = append(netem, "delay", micros(oneWay))
		if p.Jitter > 0 {
			netem = append(netem, micros(p.Jitter), "distribution", "normal")
		}
	}
	switch {
	case p.Loss > 0 && p.LossBurst > 1:
		good2bad, bad2good := p.gemodel()
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
	cmds := []Cmd{in(Router, netem...)}
	if p.Rate > 0 {
		cmds = append(cmds, in(Router, "tc", "qdisc", "add", "dev", dev, "parent", "1:1", "handle", "10:",
			"tbf", "rate", p.Rate.String(), "burst", strconv.FormatUint(tbfBurst(p.Rate), 10), "limit", strconv.Itoa(p.QueueBytes())))
	}
	return cmds
}

// tbfBurst is the token bucket size in bytes: a millisecond at the rate,
// and at least two full frames so that any packet fits.
func tbfBurst(r Rate) uint64 { return max(minQueueBytes, uint64(r)/8/1000) }

func micros(d time.Duration) string { return strconv.FormatInt(d.Microseconds(), 10) + "us" }

// percent formats p for tc with six decimals at most.
func percent(p Percent) string {
	return strconv.FormatFloat(math.Round(float64(p)*1e6)/1e6, 'f', -1, 64) + "%"
}
