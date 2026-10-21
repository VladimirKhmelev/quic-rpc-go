package loadgen

import (
	"context"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
)

// fakeClient answers after delay, or never if block is set. Under synctest
// its time is exact.
type fakeClient struct {
	delay   time.Duration
	connect time.Duration // how long Connect takes
	block   bool
	// drop says which echo messages never come back.
	drop func(seq uint64) bool
}

func (f *fakeClient) Connect(context.Context) error {
	time.Sleep(f.connect)
	return nil
}

func (f *fakeClient) Call(ctx context.Context, _ []byte, respSize int) (int, error) {
	if f.block {
		<-ctx.Done()
		return 0, &common.StatusError{Code: codeDeadlineExceeded}
	}
	select {
	case <-time.After(f.delay):
		return respSize, nil
	case <-ctx.Done():
		return 0, &common.StatusError{Code: codeDeadlineExceeded}
	}
}

func (f *fakeClient) Echo(context.Context) (common.EchoStream, error) {
	return &fakeEcho{f: f, out: make(chan [2]int, 1000)}, nil
}

func (f *fakeClient) Close() error { return nil }

type fakeEcho struct {
	f       *fakeClient
	pending sync.WaitGroup
	out     chan [2]int
}

func (e *fakeEcho) Send(seq uint64, payload []byte) error {
	if e.f.drop != nil && e.f.drop(seq) {
		return nil
	}
	e.pending.Go(func() {
		time.Sleep(e.f.delay)
		e.out <- [2]int{int(seq), len(payload)} //nolint:gosec // test sequence numbers
	})
	return nil
}

func (e *fakeEcho) Recv() (uint64, int, error) {
	m, ok := <-e.out
	if !ok {
		return 0, 0, io.EOF
	}
	return uint64(m[0]), m[1], nil //nolint:gosec // test sequence numbers
}

func (e *fakeEcho) CloseSend() error {
	go func() { e.pending.Wait(); close(e.out) }()
	return nil
}

func run(ctx context.Context, t *testing.T, cfg Config, f *fakeClient) []Record {
	t.Helper()
	var dials atomic.Int32
	if cfg.Dial == nil {
		cfg.Dial = func() (common.Client, error) { dials.Add(1); return f, nil }
	}
	records, err := Run(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return records
}

// TestOpenLoop: every call is sent on schedule and counts from it, however
// slowly the server answers.
func TestOpenLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := Profile{Name: "p", Small: &Small{Rate: 200, MinSize: 100, MaxSize: 1000}}
		cfg := Config{Profile: p, Seed: 3, Warmup: time.Second, Measure: 2 * time.Second}
		records := run(t.Context(), t, cfg, &fakeClient{delay: 500 * time.Millisecond})
		events := Schedule(p, 3, 3*time.Second)
		if len(records) != len(events) {
			t.Fatalf("%d records for %d events", len(records), len(events))
		}
		for i, r := range records {
			ev := events[i]
			if r.Seq != i || r.Kind != KindSmall || r.Scheduled != ev.At || r.Latency != 500*time.Millisecond ||
				r.Code != 0 || r.ReqBytes != ev.ReqSize || r.RespBytes != ev.RespSize || r.Warmup != (ev.At < time.Second) {
				t.Fatalf("record %d: %+v for %+v", i, r, ev)
			}
		}
	})
}

// TestTimeout: a call the server never answers ends at its deadline as
// DeadlineExceeded (GEN-9).
func TestTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := Config{Profile: Profile{Name: "p", Small: &Small{Rate: 5}}, Measure: time.Second}
		records := run(t.Context(), t, cfg, &fakeClient{block: true})
		if len(records) == 0 {
			t.Fatal("no records")
		}
		for _, r := range records {
			if r.Code != codeDeadlineExceeded || r.Latency != DefaultTimeout {
				t.Fatalf("record %+v, want DeadlineExceeded after %v", r, DefaultTimeout)
			}
		}
	})
}

func TestReconnect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := Profile{Name: "p", Small: &Small{MinSize: 10, MaxSize: 20}, Reconnect: &Reconnect{Every: 5 * time.Second, Calls: 10}}
		var dials atomic.Int32
		// The calls of a connection wait for it: they count from the time
		// the connection was due, like the connect record.
		f := &fakeClient{delay: 3 * time.Millisecond, connect: 20 * time.Millisecond}
		cfg := Config{Profile: p, Measure: 12 * time.Second, Dial: func() (common.Client, error) { dials.Add(1); return f, nil }}
		records := run(t.Context(), t, cfg, f)
		if n := dials.Load(); n != 3 {
			t.Errorf("%d dials, want 3", n)
		}
		calls := map[int]int{}
		for _, r := range records {
			switch r.Kind {
			case KindConnect:
				if r.Code != 0 || r.Latency != 20*time.Millisecond {
					t.Errorf("connect %+v", r)
				}
			case KindSmall:
				calls[r.Conn]++
				if r.Latency != 23*time.Millisecond {
					t.Errorf("call %+v", r)
				}
			default:
				t.Errorf("record %+v", r)
			}
		}
		if calls[1] != 10 || calls[2] != 10 || calls[3] != 10 {
			t.Errorf("calls per connection %v, want 10 each", calls)
		}
	})
}

// TestEcho: each message counts from when it was due to its echo; a message
// that never comes back ends as DeadlineExceeded.
func TestEcho(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := Profile{Name: "p", Echo: &Echo{Rate: 20, Size: 200}}
		f := &fakeClient{delay: 7 * time.Millisecond, drop: func(seq uint64) bool { return seq%10 == 0 }}
		records := run(t.Context(), t, Config{Profile: p, Measure: 3 * time.Second}, f)
		dropped := 0
		for _, r := range records {
			switch {
			case r.Seq%10 == 0:
				dropped++
				if r.Code != codeDeadlineExceeded || r.Latency != DefaultTimeout {
					t.Errorf("dropped message %+v", r)
				}
			case r.Code != 0 || r.Latency != 7*time.Millisecond || r.RespBytes != 200:
				t.Errorf("message %+v", r)
			}
		}
		if dropped == 0 || len(records) < 30 {
			t.Fatalf("%d records, %d dropped", len(records), dropped)
		}
	})
}

// TestCanceled: a run whose context ends records the calls it never sent
// as Canceled and returns the context's error.
func TestCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
		defer cancel()
		p := Profile{Name: "p", Small: &Small{Rate: 100}}
		records, err := Run(ctx, Config{Profile: p, Measure: 3 * time.Second,
			Dial: func() (common.Client, error) { return &fakeClient{delay: time.Millisecond}, nil }})
		if err == nil {
			t.Fatal("no error from a canceled run")
		}
		for _, r := range records {
			if want := r.Scheduled >= 1500*time.Millisecond; (r.Code == codeCanceled) != want {
				t.Fatalf("record %+v", r)
			}
		}
	})
}

// TestClosedLoop: each worker sends the next call when the last one is
// answered.
func TestClosedLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := Profile{Name: "p", Small: &Small{Rate: 1000, MinSize: 1, MaxSize: 1}}
		records := run(t.Context(), t, Config{Profile: p, Measure: time.Second, Closed: 1}, &fakeClient{delay: 10 * time.Millisecond})
		if len(records) != 100 {
			t.Fatalf("%d calls of 10 ms in 1 s on one worker", len(records))
		}
		for i, r := range records {
			if r.Seq != i || r.Scheduled != time.Duration(i)*10*time.Millisecond || r.Latency != 10*time.Millisecond {
				t.Fatalf("record %+v", r)
			}
		}
	})
}

// TestPhases: the phases begin on time, and a run lasts its full length
// even when its last call is answered earlier.
func TestPhases(t *testing.T) {
	for _, closed := range []int{0, 1} {
		synctest.Test(t, func(t *testing.T) {
			start := time.Now()
			var phases []Phase
			var at []time.Duration
			cfg := Config{
				Profile: Profile{Name: "p", Small: &Small{Rate: 2, MinSize: 1, MaxSize: 1}},
				Warmup:  time.Second, Measure: 2 * time.Second, Closed: closed,
				OnPhase: func(p Phase) { phases = append(phases, p); at = append(at, time.Since(start)) },
			}
			run(t.Context(), t, cfg, &fakeClient{delay: time.Millisecond})
			if want := []Phase{PhaseWarmup, PhaseMeasure, PhaseEnd}; !slices.Equal(phases, want) {
				t.Fatalf("closed %d: phases %v, want %v", closed, phases, want)
			}
			if want := []time.Duration{0, time.Second, 3 * time.Second}; !slices.Equal(at, want) {
				t.Errorf("closed %d: phases at %v, want %v", closed, at, want)
			}
			if d := time.Since(start); d < 3*time.Second {
				t.Errorf("closed %d: the run took %v", closed, d)
			}
		})
	}
}

// TestPhasesCanceled: a run canceled during the measurement never ends it.
func TestPhasesCanceled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		var phases []Phase
		_, err := Run(ctx, Config{
			Profile: Profile{Name: "p", Small: &Small{Rate: 2}}, Warmup: time.Second, Measure: 2 * time.Second,
			Dial:    func() (common.Client, error) { return &fakeClient{}, nil },
			OnPhase: func(p Phase) { phases = append(phases, p) },
		})
		if err == nil || !slices.Equal(phases, []Phase{PhaseWarmup, PhaseMeasure}) {
			t.Fatalf("phases %v, %v", phases, err)
		}
	})
}
