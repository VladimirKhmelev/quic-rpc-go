//go:build linux

package netns

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/scenarios"
)

// unavailable is why the testbed cannot run on this machine, if it cannot.
var unavailable error

// TestMain runs the tests in namespaces of their own (see Enter). Where
// user namespaces are disabled the testbed tests are skipped, unless
// QRPC_TESTBED_REQUIRED is set, as in CI.
func TestMain(m *testing.M) {
	if err := Enter(); err != nil {
		if os.Getenv("QRPC_TESTBED_REQUIRED") != "" {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		unavailable = err
	}
	os.Exit(m.Run())
}

func up(t *testing.T, p Profile) *Stand {
	t.Helper()
	if unavailable != nil {
		t.Skip(unavailable)
	}
	s := &Stand{}
	if err := s.Up(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Down(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return s
}

func output(t *testing.T, ns, name string, args ...string) string {
	t.Helper()
	out, err := Command(t.Context(), ns, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %s %s: %v\n%s", ns, name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

var pingTime = regexp.MustCompile(`time=([0-9.]+) ms`)

// pingRTT returns the median round-trip time from the client to the server.
func pingRTT(t *testing.T, count int) time.Duration {
	t.Helper()
	out := output(t, Client, "ping", "-n", "-c", strconv.Itoa(count), "-i", "0.01", ServerAddr.String())
	var rtts []time.Duration
	for _, m := range pingTime.FindAllStringSubmatch(out, -1) {
		ms, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatalf("ping time %q: %v", m[1], err)
		}
		rtts = append(rtts, time.Duration(ms*float64(time.Millisecond)))
	}
	if len(rtts) != count {
		t.Fatalf("got %d replies to %d pings:\n%s", len(rtts), count, out)
	}
	slices.Sort(rtts)
	return rtts[len(rtts)/2]
}

func TestPingThroughRouter(t *testing.T) {
	up(t, None)
	out := output(t, Client, "ping", "-n", "-c", "3", "-i", "0.01", ServerAddr.String())
	// One hop: the router decrements the TTL of 64 once.
	if !strings.Contains(out, "ttl=63") {
		t.Fatalf("replies did not come through the router:\n%s", out)
	}
	for _, ns := range []string{Client, Router, Server} {
		want := map[bool]string{true: "1\n", false: "0\n"}[ns == Router]
		if got := output(t, ns, "sysctl", "-n", "net.ipv4.ip_forward"); got != want {
			t.Errorf("%s: ip_forward is %q, want %q", ns, got, want)
		}
		for line := range strings.Lines(output(t, ns, "ip", "neigh", "show")) {
			if !strings.Contains(line, "PERMANENT") {
				t.Errorf("%s has a learned neighbor: %s", ns, line)
			}
		}
	}
}

// TestRTT checks NET-3: half the RTT in each direction adds up to the RTT
// of the profile, within the 10% tolerance of the self-check.
func TestRTT(t *testing.T) {
	const rtt = 40 * time.Millisecond
	up(t, Profile{Name: "rtt", RTT: rtt})
	if got := pingRTT(t, 20); got < rtt || got > rtt+rtt/10 {
		t.Fatalf("median RTT %v, want %v within 10%%", got, rtt)
	}
}

func TestOffloads(t *testing.T) {
	up(t, None)
	for _, l := range Links {
		for _, e := range []End{l.A, l.B} {
			out := output(t, e.NS, "ethtool", "-k", e.Dev)
			want := map[string]string{
				"tcp-segmentation-offload":     "off",
				"generic-segmentation-offload": "off",
				"generic-receive-offload":      map[bool]string{true: "off", false: "on"}[e.NS == Router],
			}
			for feature, state := range want {
				if !strings.Contains(out, feature+": "+state) {
					t.Errorf("%s %s: %s is not %s", e.NS, e.Dev, feature, state)
				}
			}
		}
	}
}

func TestApplyReplacesImpairments(t *testing.T) {
	s := up(t, Profile{Name: "slow", RTT: 150 * time.Millisecond, Rate: 100e6, QueueBDP: 1})
	qdiscs := func() string {
		return output(t, Router, "tc", "qdisc", "show", "dev", "fromclient") +
			output(t, Router, "tc", "qdisc", "show", "dev", "toserver")
	}
	if q := qdiscs(); !strings.Contains(q, "tbf") || !strings.Contains(q, "delay 75ms") {
		t.Fatalf("after Up:\n%s", q)
	}
	near := Profile{Name: "near", RTT: 10 * time.Millisecond}
	for range 2 {
		if err := s.Apply(t.Context(), near); err != nil {
			t.Fatal(err)
		}
		if q := qdiscs(); strings.Contains(q, "tbf") || !strings.Contains(q, "delay 5ms") || !strings.Contains(q, "noqueue") {
			t.Fatalf("after Apply:\n%s", q)
		}
	}
	if got := pingRTT(t, 10); got < near.RTT || got > near.RTT+near.RTT/10 {
		t.Fatalf("median RTT %v after Apply, want %v", got, near.RTT)
	}
}

// TestBundledProfilesApply checks that the kernel takes every bundled
// profile.
func TestBundledProfilesApply(t *testing.T) {
	s := up(t, None)
	profiles, err := ParseProfiles(scenarios.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range profiles {
		if err := s.Apply(t.Context(), p); err != nil {
			t.Fatalf("profile %s: %v", p.Name, err)
		}
		for _, path := range Paths {
			q := output(t, Router, "tc", "qdisc", "show", "dev", path.Ifb) + output(t, Router, "tc", "qdisc", "show", "dev", path.Out)
			if strings.Contains(q, "tbf") != (p.Rate > 0) || !strings.Contains(q, "netem") {
				t.Errorf("profile %s, %s:\n%s", p.Name, path.Name, q)
			}
		}
	}
}

// TestDo opens a socket in each end namespace and sends a datagram from one
// to the other through the router.
func TestDo(t *testing.T) {
	up(t, None)
	listen := func(ns string, addr netip.Addr) *net.UDPConn {
		t.Helper()
		var conn *net.UDPConn
		err := Do(ns, func() (err error) {
			conn, err = net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr, 0)))
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
	client, server := listen(Client, ClientAddr), listen(Server, ServerAddr)
	if _, err := client.WriteTo([]byte("hello"), server.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	if err := server.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	n, from, err := server.ReadFromUDPAddrPort(buf)
	if err != nil || string(buf[:n]) != "hello" || from.Addr() != ClientAddr {
		t.Fatalf("server read %q from %v, %v", buf[:n], from, err)
	}

	// The caller stays outside: here only lo exists.
	ifaces, err := net.Interfaces()
	if err != nil || len(ifaces) != 1 {
		t.Fatalf("interfaces after Do: %v, %v", ifaces, err)
	}
	if err := Do("missing", func() error { return nil }); err == nil {
		t.Fatal("Do in a missing namespace succeeded")
	}
}

var qdiscStats = regexp.MustCompile(`Sent \d+ bytes (\d+) pkt \(dropped (\d+), overlimits \d+ requeues (\d+)\)`)

// stats returns the packets sent and dropped by the root qdisc of dev and
// its requeues, which netem counts for the packets it reorders.
func stats(t *testing.T, dev string) (sent, dropped, requeues int) {
	t.Helper()
	out := output(t, Router, "tc", "-s", "qdisc", "show", "dev", dev)
	m := qdiscStats.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no statistics for %s:\n%s", dev, out)
	}
	n := func(s string) int { v, _ := strconv.Atoi(s); return v }
	return n(m[1]), n(m[2]), n(m[3])
}

// TestReorderedPacketsPassTheBottleneck sends a burst through a profile
// that reorders and has a bottleneck: every packet netem lets out, the
// reordered ones too, must then pass the tbf, sent or dropped.
func TestReorderedPacketsPassTheBottleneck(t *testing.T) {
	p := Profile{Name: "reorder", RTT: 10 * time.Millisecond, Reorder: 20, Rate: 10e6, QueueBDP: 1}
	up(t, p)
	dst := netip.AddrPortFrom(ServerAddr, 9)
	var sink, conn *net.UDPConn
	err := Do(Server, func() (err error) {
		sink, err = net.ListenUDP("udp", net.UDPAddrFromAddrPort(dst))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close() }()
	err = Do(Client, func() (err error) {
		conn, err = net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(dst))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	for range 2000 {
		if _, err := conn.Write(make([]byte, 100)); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(p.RTT + 200*time.Millisecond) // the queues drain
	released, _, reordered := stats(t, Paths[0].Ifb)
	sent, dropped, _ := stats(t, Paths[0].Out)
	if reordered == 0 {
		t.Fatalf("netem reordered nothing of %d packets", released)
	}
	if sent+dropped != released {
		t.Fatalf("netem let out %d packets (%d reordered), tbf took %d: %d sent, %d dropped",
			released, reordered, sent+dropped, sent, dropped)
	}
}

// TestOutages sends a datagram every 5 ms through a profile that cuts the
// link for 400 ms every second: nothing sent well inside the first outage
// arrives, almost everything before and after it does.
func TestOutages(t *testing.T) {
	p := Profile{Name: "outages", RTT: 10 * time.Millisecond, Outage: Outage{Duration: 400 * time.Millisecond, Every: time.Second}}
	s := up(t, p)
	dst := netip.AddrPortFrom(ServerAddr, 9)
	var sink, conn *net.UDPConn
	if err := Do(Server, func() (err error) { sink, err = net.ListenUDP("udp", net.UDPAddrFromAddrPort(dst)); return err }); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sink.Close() }()
	if err := Do(Client, func() (err error) { conn, err = net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(dst)); return err }); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	outages := make(chan error, 1)
	go func() { outages <- s.Outages(ctx, p) }()
	const interval, total = 5 * time.Millisecond, 1900 * time.Millisecond
	sent := int(total / interval)
	// Read while sending: the socket buffer would not hold them all.
	got := make([]bool, sent)
	read := make(chan struct{})
	go func() {
		defer close(read)
		for buf := make([]byte, 2); ; {
			n, err := sink.Read(buf)
			if err != nil {
				return
			}
			if i := int(buf[0])<<8 | int(buf[1]); n == 2 && i < sent {
				got[i] = true
			}
		}
	}()
	start := time.Now()
	for i := range sent {
		time.Sleep(time.Until(start.Add(time.Duration(i) * interval)))
		if _, err := conn.Write([]byte{byte(i >> 8), byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	// Late datagrams arrive within the delay; reading stops at the deadline.
	if err := sink.SetReadDeadline(time.Now().Add(p.RTT + 200*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	<-read
	share := func(from, to time.Duration) float64 {
		n := 0
		for i := int(from / interval); i < int(to/interval); i++ {
			if got[i] {
				n++
			}
		}
		return float64(n) / float64((to-from)/interval)
	}
	// The first outage lasts from 1000 to 1400 ms, the next starts at 2000.
	if before, during, after := share(200*time.Millisecond, 900*time.Millisecond),
		share(1100*time.Millisecond, 1300*time.Millisecond), share(1550*time.Millisecond, 1850*time.Millisecond); before < 0.9 || during > 0 || after < 0.9 {
		t.Fatalf("arrived: %.0f%% before the outage, %.0f%% during it, %.0f%% after it", 100*before, 100*during, 100*after)
	}
	cancel()
	if err := <-outages; err != nil {
		t.Fatal(err)
	}
}

// TestOutageEndsWithContext cancels the schedule during an outage: the
// impairments of the profile must come back.
func TestOutageEndsWithContext(t *testing.T) {
	p := Profile{Name: "cut", RTT: 10 * time.Millisecond, Outage: Outage{Duration: 50 * time.Millisecond, Every: 100 * time.Millisecond}}
	s := up(t, p)
	ctx, cancel := context.WithCancel(t.Context())
	outages := make(chan error, 1)
	go func() { outages <- s.Outages(ctx, p) }()
	// Inside the first outage, between 100 and 150 ms.
	time.Sleep(125 * time.Millisecond)
	cancel()
	if err := <-outages; err != nil {
		t.Fatal(err)
	}
	for _, path := range Paths {
		if q := output(t, Router, "tc", "qdisc", "show", "dev", path.Ifb); strings.Contains(q, "loss 100%") || !strings.Contains(q, "delay 5ms") {
			t.Errorf("%s after the schedule ended:\n%s", path.Ifb, q)
		}
	}
}

// TestTCPSetupsTakeEffect checks that the kernel took the TCP setup of the
// end hosts.
func TestTCPSetupsTakeEffect(t *testing.T) {
	if unavailable != nil {
		t.Skip(unavailable)
	}
	for _, tcp := range []TCP{TCPAligned, TCPDefault} {
		t.Run(string(tcp), func(t *testing.T) {
			s := &Stand{TCP: tcp}
			if err := s.Up(t.Context(), None); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Down(context.Background()) }()
			aligned := tcp == TCPAligned
			for _, ns := range []string{Client, Server} {
				route := output(t, ns, "ip", "route", "show", "default")
				qdisc := output(t, ns, "tc", "qdisc", "show", "dev", "eth0")
				idle := output(t, ns, "sysctl", "-n", "net.ipv4.tcp_slow_start_after_idle")
				if strings.Contains(route, "initcwnd 32 initrwnd 32") != aligned ||
					strings.Contains(qdisc, "qdisc fq ") != aligned ||
					idle != map[bool]string{true: "0\n", false: "1\n"}[aligned] {
					t.Errorf("%s:\n%s%s slow start after idle: %s", ns, route, qdisc, idle)
				}
				if got := output(t, ns, "sysctl", "-n", "net.ipv4.tcp_no_metrics_save"); got != "1\n" {
					t.Errorf("%s: tcp_no_metrics_save %q", ns, got)
				}
			}
			// TCP works through the setup: a connection is accepted.
			var ln net.Listener
			err := Do(Server, func() (err error) {
				ln, err = net.Listen("tcp", netip.AddrPortFrom(ServerAddr, 7).String())
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = ln.Close() }()
			var conn net.Conn
			err = Do(Client, func() (err error) {
				conn, err = net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
		})
	}
	if err := (&Stand{TCP: "tuned"}).Up(t.Context(), None); err == nil {
		t.Error("Up took an unknown TCP setup")
	}
}

func TestDown(t *testing.T) {
	if unavailable != nil {
		t.Skip(unavailable)
	}
	s := &Stand{}
	for range 2 { // the second round shows that Down left nothing behind
		if err := s.Up(t.Context(), None); err != nil {
			t.Fatal(err)
		}
		if err := s.Down(t.Context()); err != nil {
			t.Fatal(err)
		}
		if out := output(t, "", "ip", "netns", "list"); out != "" {
			t.Fatalf("namespaces after Down:\n%s", out)
		}
	}
}
