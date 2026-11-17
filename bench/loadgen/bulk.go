package loadgen

import (
	"context"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
)

// runBulk measures a finite amount of work instead of a fixed time window.
// Every warm-up attempt remains in the raw data; a failed warm-up is not
// repeated to manufacture success. One measured attempt always follows it.
func runBulk(ctx context.Context, cfg Config) ([]Record, error) {
	b := cfg.Profile.Bulk
	c, err := cfg.Dial()
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	connectTimeout := cfg.Timeout
	if connectTimeout == 0 {
		connectTimeout = DefaultTimeout
	}
	connectCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	_ = c.Connect(connectCtx)
	cancel()
	phase := cfg.OnPhase
	if phase == nil {
		phase = func(Phase) {}
	}
	phase(PhaseWarmup)
	start := time.Now()
	var records []Record
	attempt := func(warmup bool) Record {
		due := time.Now()
		callCtx, cancel := context.WithDeadline(ctx, due.Add(b.Deadline()))
		defer cancel()
		result, err := common.ReceiveBulk(callCtx, c, b.TotalBytes, b.ChunkBytes)
		return Record{Seq: len(records), Kind: KindBulk, Scheduled: due.Sub(start),
			Latency: time.Since(due), Code: common.Code(err), RespBytes: result.Bytes,
			Chunks: result.Chunks, Warmup: warmup}
	}
	for time.Since(start) < cfg.Warmup && ctx.Err() == nil {
		r := attempt(true)
		records = append(records, r)
		if r.Code != codeOK {
			break
		}
	}
	phase(PhaseMeasure)
	records = append(records, attempt(false))
	phase(PhaseEnd)
	return records, ctx.Err()
}
