package loadgen

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
)

type bulkClient struct {
	fakeClient
	delay time.Duration
}

func (c *bulkClient) Bulk(ctx context.Context, total, chunk int) (common.BulkStream, error) {
	return &delayedBulk{ctx: ctx, left: total, chunk: chunk, delay: c.delay}, nil
}

type delayedBulk struct {
	ctx   context.Context
	left  int
	chunk int
	seq   uint64
	delay time.Duration
}

func (s *delayedBulk) Recv() (uint64, int, error) {
	select {
	case <-s.ctx.Done():
		code := uint32(1)
		if errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
			code = 4
		}
		return 0, 0, &common.StatusError{Code: code}
	case <-time.After(s.delay):
	}
	if s.left == 0 {
		return 0, 0, io.EOF
	}
	n, seq := min(s.left, s.chunk), s.seq
	s.left -= n
	s.seq++
	return seq, n, nil
}

func TestBulkFiniteWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		phases := map[Phase]time.Duration{}
		c := &bulkClient{delay: 10 * time.Millisecond}
		records, err := Run(t.Context(), Config{
			Profile: Profile{Name: "bulk", Bulk: &Bulk{TotalBytes: 2, ChunkBytes: 1}},
			Warmup:  50 * time.Millisecond, Measure: time.Hour,
			Dial:    func() (common.Client, error) { return c, nil },
			OnPhase: func(p Phase) { phases[p] = time.Since(start) },
		})
		if err != nil || len(records) != 3 {
			t.Fatalf("records=%+v err=%v", records, err)
		}
		for i, r := range records {
			if r.Warmup != (i < 2) || r.RespBytes != 2 || r.Chunks != 2 || r.Code != 0 || r.Latency != 30*time.Millisecond {
				t.Fatalf("record %d: %+v", i, r)
			}
		}
		if phases[PhaseMeasure] != 60*time.Millisecond || phases[PhaseEnd] != 90*time.Millisecond || records[2].Scheduled != phases[PhaseMeasure] {
			t.Fatalf("phases=%v records=%+v", phases, records)
		}
	})
}

func TestBulkPartialDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &bulkClient{delay: 10 * time.Millisecond}
		records, err := Run(t.Context(), Config{
			Profile: Profile{Name: "bulk", Bulk: &Bulk{TotalBytes: 2, ChunkBytes: 1, Timeout: 15 * time.Millisecond}},
			Measure: time.Hour, Dial: func() (common.Client, error) { return c, nil },
		})
		if err != nil || len(records) != 1 || records[0].Code != 4 || records[0].RespBytes != 1 || records[0].Chunks != 1 || records[0].Latency != 15*time.Millisecond {
			t.Fatalf("records=%+v err=%v", records, err)
		}
	})
}

func TestBulkProfileValidation(t *testing.T) {
	for _, b := range []Bulk{
		{TotalBytes: 0, ChunkBytes: 1}, {TotalBytes: 1, ChunkBytes: 0},
		{TotalBytes: 1, ChunkBytes: 4 << 20}, {TotalBytes: 1, ChunkBytes: 1, Timeout: -1},
	} {
		if err := (Profile{Name: "bulk", Bulk: &b}).Validate(); err == nil {
			t.Fatalf("accepted %+v", b)
		}
	}
	p := Profile{Name: "bulk", Bulk: &Bulk{TotalBytes: 100 << 20, ChunkBytes: 64 << 10}}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	p.Small = &Small{Rate: 1}
	if err := p.Validate(); err == nil {
		t.Fatal("accepted mixed bulk profile")
	}
	if (Bulk{}).Deadline() != time.Minute {
		t.Fatal("wrong default timeout")
	}
}
