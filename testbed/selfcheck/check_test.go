//go:build linux

package selfcheck

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
)

var unavailable error

// TestMain runs the tests in a testbed of their own, as netns does.
func TestMain(m *testing.M) {
	if err := netns.Enter(); err != nil {
		if os.Getenv("QRPC_TESTBED_REQUIRED") != "" {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		unavailable = err
	}
	os.Exit(m.Run())
}

// fast keeps the probes short with a floor below that of NET-4; for the
// profiles with losses below the formulas still size the probes.
var fast = Options{MinPackets: 20000, MaxPacketRate: 10000}

func upWith(t *testing.T, p netns.Profile) {
	t.Helper()
	if unavailable != nil {
		t.Skip(unavailable)
	}
	s := &netns.Stand{}
	if err := s.Up(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Down(context.Background()); err != nil {
			t.Error(err)
		}
	})
}

func TestRunMatchesTestbed(t *testing.T) {
	tests := []struct {
		p          netns.Profile
		needsIperf bool
	}{
		// NET-2: jitter alone leaves the order intact, and the plain
		// delay does not reorder either.
		{netns.Profile{Name: "jitter", RTT: 20 * time.Millisecond, Jitter: 2 * time.Millisecond}, false},
		{netns.Profile{Name: "impaired", RTT: 20 * time.Millisecond, Jitter: 2 * time.Millisecond,
			Loss: 5, LossBurst: 2, Reorder: 5, Rate: 50e6, QueueBDP: 1}, true},
	}
	for _, tt := range tests {
		t.Run(tt.p.Name, func(t *testing.T) {
			if _, err := exec.LookPath("iperf3"); err != nil && tt.needsIperf && os.Getenv("QRPC_TESTBED_REQUIRED") == "" {
				t.Skip("iperf3 is not installed")
			}
			upWith(t, tt.p)
			r, err := Run(t.Context(), tt.p, fast)
			if err != nil {
				t.Fatal(err)
			}
			if !r.OK() {
				t.Fatalf("self-check failed on a matching testbed:\n%v", r)
			}
			names := map[string]bool{}
			for _, c := range r.Checks {
				names[c.Name] = true
			}
			want := []string{"rtt", "loss", "reorder"}
			if tt.needsIperf {
				want = append(want, "loss burst", "bandwidth")
			}
			for _, name := range want {
				if !names[name] {
					t.Errorf("no %s check:\n%v", name, r)
				}
			}
		})
	}
}

// TestRunDetectsMismatch runs the self-check against a profile the testbed
// does not have: every differing value must fail.
func TestRunDetectsMismatch(t *testing.T) {
	upWith(t, netns.Profile{Name: "actual", RTT: 20 * time.Millisecond, Loss: 5, Reorder: 5})
	target := netns.Profile{Name: "target", RTT: 30 * time.Millisecond, Loss: 2.5}
	r, err := Run(t.Context(), target, fast)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK() || len(r.Invalid) > 0 {
		t.Fatalf("want a valid report out of tolerance:\n%v", r)
	}
	for _, c := range r.Checks {
		if c.OK {
			t.Errorf("%s %s passed against the wrong target:\n%v", c.Name, c.Direction, r)
		}
	}
}

// TestRunFlagsOverloadedProbe makes the probe drop packets in its own
// socket buffer: the report must call the measurement invalid instead of
// blaming the testbed.
func TestRunFlagsOverloadedProbe(t *testing.T) {
	upWith(t, netns.None)
	opts := fast
	opts.MaxPacketRate = 1e6 // a thousand packets per millisecond
	opts.rcvbuf = 1          // the kernel's minimum
	r, err := Run(t.Context(), netns.None, opts)
	if err != nil {
		t.Fatal(err)
	}
	if r.OK() || !strings.Contains(strings.Join(r.Invalid, "\n"), "RcvbufErrors") {
		t.Fatalf("want an invalid report blaming the socket buffer:\n%v", r)
	}
}
