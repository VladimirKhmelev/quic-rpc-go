//go:build linux

package netns

import (
	"context"
	"fmt"
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

func TestOffloadsOff(t *testing.T) {
	up(t, None)
	for _, l := range Links {
		for _, e := range []End{l.A, l.B} {
			out := output(t, e.NS, "ethtool", "-k", e.Dev)
			for _, feature := range []string{"tcp-segmentation-offload", "generic-segmentation-offload", "generic-receive-offload"} {
				if !strings.Contains(out, feature+": off") {
					t.Errorf("%s %s: %s is not off", e.NS, e.Dev, feature)
				}
			}
		}
	}
}

func TestApplyReplacesImpairments(t *testing.T) {
	s := up(t, Profile{Name: "slow", RTT: 150 * time.Millisecond, Rate: 100e6, QueueBDP: 1})
	qdiscs := func() string { return output(t, Router, "tc", "qdisc", "show", "dev", "toserver") }
	if q := qdiscs(); !strings.Contains(q, "tbf") || !strings.Contains(q, "delay 75ms") {
		t.Fatalf("after Up:\n%s", q)
	}
	near := Profile{Name: "near", RTT: 10 * time.Millisecond}
	for range 2 {
		if err := s.Apply(t.Context(), near); err != nil {
			t.Fatal(err)
		}
		if q := qdiscs(); strings.Contains(q, "tbf") || !strings.Contains(q, "delay 5ms") {
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
		for _, dev := range impairedDevs {
			q := output(t, Router, "tc", "qdisc", "show", "dev", dev)
			if strings.Contains(q, "tbf") != (p.Rate > 0) || !strings.Contains(q, "netem") {
				t.Errorf("profile %s, %s:\n%s", p.Name, dev, q)
			}
		}
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
