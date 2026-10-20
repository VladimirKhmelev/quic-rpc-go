package loadgen

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
	benchv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1/benchv1quic"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/mem"
)

// delayServer answers every call after delay. Calls that arrive between
// stallFrom and stallTo wait until stallTo first, as behind a server that
// froze for that time.
type delayServer struct {
	benchv1quic.UnimplementedBenchServer
	delay              time.Duration
	stallFrom, stallTo time.Time
}

func (s *delayServer) Call(_ context.Context, req *benchv1.CallRequest) (*benchv1.CallResponse, error) {
	if now := time.Now(); !now.Before(s.stallFrom) && now.Before(s.stallTo) {
		time.Sleep(s.stallTo.Sub(now))
	}
	time.Sleep(s.delay)
	return &benchv1.CallResponse{Payload: make([]byte, req.GetResponseSize())}, nil
}

// tst8 runs the small profile for 1 s of warm-up and 30 s of measurement
// against a delayServer of quic-rpc-go over the in-memory transport. Under
// synctest nothing but the server's delays takes time, so the latencies
// are known exactly.
func tst8(t *testing.T, stall bool, closed int) KindSummary {
	t.Helper()
	var sum KindSummary
	synctest.Test(t, func(t *testing.T) {
		l := mem.Listen(mem.Options{})
		srv := &delayServer{delay: 10 * time.Millisecond}
		if stall {
			// A second-long freeze in the middle of the measurement.
			srv.stallFrom = time.Now().Add(10 * time.Second)
			srv.stallTo = srv.stallFrom.Add(time.Second)
		}
		s := rpc.NewServer()
		benchv1quic.RegisterBenchServer(s, srv)
		go func() { _ = s.Serve(l) }()
		defer s.Stop()

		profiles, err := ParseProfiles([]byte("loads:\n  - name: small\n    small: {rate: 200, min_size: 100, max_size: 1000}\n"))
		if err != nil {
			t.Fatal(err)
		}
		records, err := Run(t.Context(), Config{
			Profile: profiles[0], Seed: 1, Warmup: time.Second, Measure: 30 * time.Second, Closed: closed,
			Dial: func() (common.Client, error) { return common.NewRPCClient(l), nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		sums := Summarize(records)
		if len(sums) != 1 || sums[0].Errors != 0 {
			t.Fatalf("summary %+v", sums)
		}
		sum = sums[0]
	})
	return sum
}

// TestTST8 checks the generator against a server of known latency (TST-8):
// the measured p50 and p99 equal it, and a stall of the server shows in the
// tail of an open-loop run but hardly in a closed-loop one, which sends
// nothing while it waits.
func TestTST8(t *testing.T) {
	if s := tst8(t, false, 0); s.P50 != 10*time.Millisecond || s.P99 != 10*time.Millisecond {
		t.Errorf("steady open loop: p50 %v, p99 %v, want 10ms both", s.P50, s.P99)
	}
	// Open loop: about 200 of the 6000 measured calls are due during the
	// stall, each waiting out the rest of it. p99 is the 60th longest
	// latency: a call due 0.3 s into the stall, about 0.7 s.
	open := tst8(t, true, 0)
	// Closed loop: one call waits out the stall among some 3000.
	closed := tst8(t, true, 1)
	t.Logf("with a 1 s stall: open loop %d calls, p99 %v; closed loop %d calls, p99 %v", open.Count, open.P99, closed.Count, closed.P99)
	if open.P99 < 500*time.Millisecond || open.P99 > 900*time.Millisecond {
		t.Errorf("open loop p99 %v, want about 0.7s", open.P99)
	}
	if closed.P99 != 10*time.Millisecond {
		t.Errorf("closed loop p99 %v, want the 10ms of an unstalled call", closed.P99)
	}
}
