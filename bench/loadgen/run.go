package loadgen

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
)

// DefaultTimeout is the deadline of a call, counted from the time it was due.
const DefaultTimeout = 10 * time.Second

// Status codes, numbered as in gRPC.
const (
	codeOK               = 0
	codeCanceled         = 1
	codeDeadlineExceeded = 4
)

// Config is one run of the generator.
type Config struct {
	Profile Profile
	Seed    uint64
	// Calls due before Warmup are recorded as warm-up; the run lasts
	// Warmup + Measure.
	Warmup, Measure time.Duration
	// Timeout is the deadline of every call; zero means DefaultTimeout.
	Timeout time.Duration
	// Dial makes a client. Run calls it once, and once per connection of a
	// reconnect profile.
	Dial func() (common.Client, error)
	// Closed, if above zero, runs the small and heavy calls closed-loop
	// with that many workers, each sending the next call once the last one
	// is answered, instead of on the schedule.
	Closed int
}

// Record is one call, message or connection setup.
type Record struct {
	Seq  int
	Kind Kind
	// Scheduled is when the call was due, since the start of the run;
	// Latency runs from there to the answer or the failure.
	Scheduled time.Duration
	Latency   time.Duration
	Code      uint32
	ReqBytes  int
	RespBytes int
	Conn      int
	Warmup    bool
}

type runner struct {
	cfg     Config
	timeout time.Duration
	start   time.Time
	events  []Event
	records []Record
}

// Run makes the calls of cfg and returns a record of each, ordered as
// scheduled. It returns an error only if it cannot run at all; calls that
// fail are records with their status code.
func Run(ctx context.Context, cfg Config) ([]Record, error) {
	if err := cfg.Profile.Validate(); err != nil {
		return nil, err
	}
	if cfg.Dial == nil || cfg.Measure <= 0 || cfg.Warmup < 0 {
		return nil, errors.New("loadgen: want Dial, a positive Measure and no negative Warmup")
	}
	r := &runner{cfg: cfg, timeout: cfg.Timeout}
	if r.timeout == 0 {
		r.timeout = DefaultTimeout
	}
	total := cfg.Warmup + cfg.Measure
	if cfg.Closed > 0 {
		return r.closed(ctx, total)
	}
	r.events = Schedule(cfg.Profile, cfg.Seed, total)
	r.records = make([]Record, len(r.events))

	var main common.Client
	if cfg.Profile.Reconnect == nil {
		var err error
		if main, err = cfg.Dial(); err != nil {
			return nil, err
		}
		defer func() { _ = main.Close() }()
		// Set up before the start: the schedule measures calls, not the
		// first handshake. A failure shows in the calls.
		connectCtx, cancel := context.WithTimeout(ctx, r.timeout)
		_ = main.Connect(connectCtx)
		cancel()
	}

	r.start = time.Now()
	var wg sync.WaitGroup
	if r.cfg.Profile.Echo != nil {
		wg.Go(func() { r.echo(ctx, main) })
	}
	for i, ev := range r.events {
		if ev.Kind == KindEcho || ev.Kind == KindSmall && ev.Conn > 0 {
			continue // run by r.echo and by their reconnection
		}
		if !sleepUntil(ctx, r.start.Add(ev.At)) {
			break
		}
		switch ev.Kind {
		case KindSmall, KindHeavy:
			wg.Go(func() { r.records[i] = r.call(ctx, main, i, ev) })
		case KindConnect:
			wg.Go(func() { r.reconnect(ctx, i, ev) })
		}
	}
	wg.Wait()
	r.cancelUnsent(ctx)
	return r.records, ctx.Err()
}

// call makes the unary call of ev on c.
func (r *runner) call(ctx context.Context, c common.Client, seq int, ev Event) Record {
	due := r.start.Add(ev.At)
	ctx, cancel := context.WithDeadline(ctx, due.Add(r.timeout))
	defer cancel()
	n, err := c.Call(ctx, payload(ev.ReqSize), ev.RespSize)
	return r.record(seq, ev, time.Since(due), common.Code(err), n)
}

// reconnect sets up connection ev.Conn and makes its calls on it at once.
func (r *runner) reconnect(ctx context.Context, seq int, ev Event) {
	due := r.start.Add(ev.At)
	c, err := r.cfg.Dial()
	if err != nil {
		r.records[seq] = r.record(seq, ev, 0, common.Code(err), 0)
		return
	}
	defer func() { _ = c.Close() }()
	connectCtx, cancel := context.WithDeadline(ctx, due.Add(r.timeout))
	err = c.Connect(connectCtx)
	cancel()
	r.records[seq] = r.record(seq, ev, time.Since(due), common.Code(err), 0)
	var wg sync.WaitGroup
	for i := seq + 1; i < len(r.events) && r.events[i].Conn == ev.Conn && r.events[i].Kind == KindSmall; i++ {
		wg.Go(func() { r.records[i] = r.call(ctx, c, i, r.events[i]) })
	}
	wg.Wait()
}

// echo sends the echo messages on their schedule over one stream of c and
// records the time each comes back.
func (r *runner) echo(ctx context.Context, c common.Client) {
	var seqs []int
	for i, ev := range r.events {
		if ev.Kind == KindEcho {
			seqs = append(seqs, i)
		}
	}
	if len(seqs) == 0 {
		return
	}
	// Every message may take up to the timeout; the stream lives as long.
	last := r.start.Add(r.events[seqs[len(seqs)-1]].At)
	ctx, cancel := context.WithDeadline(ctx, last.Add(r.timeout))
	defer cancel()
	failAll := func(code uint32) {
		for _, i := range seqs {
			r.records[i] = r.record(i, r.events[i], 0, code, 0)
		}
	}
	s, err := c.Echo(ctx)
	if err != nil {
		failAll(common.Code(err))
		return
	}
	var mu sync.Mutex
	answered := make(map[int]bool, len(seqs))
	received := make(chan struct{})
	go func() {
		defer close(received)
		for {
			seq, size, err := s.Recv()
			if err != nil {
				return
			}
			mu.Lock()
			if i := int(seq); i < len(r.events) && !answered[i] { //nolint:gosec // our own sequence numbers
				answered[i] = true
				r.records[i] = r.record(i, r.events[i], time.Since(r.start.Add(r.events[i].At)), codeOK, size)
			}
			mu.Unlock()
		}
	}()
	sendErr := error(nil)
	for _, i := range seqs {
		ev := r.events[i]
		if !sleepUntil(ctx, r.start.Add(ev.At)) {
			break
		}
		if sendErr == nil {
			sendErr = s.Send(uint64(i), payload(ev.ReqSize)) //nolint:gosec // an index
		}
		if sendErr != nil {
			mu.Lock()
			answered[i] = true
			r.records[i] = r.record(i, ev, time.Since(r.start.Add(ev.At)), common.Code(sendErr), 0)
			mu.Unlock()
		}
	}
	_ = s.CloseSend()
	<-received // io.EOF once all is echoed, or the deadline
	mu.Lock()
	defer mu.Unlock()
	for _, i := range seqs {
		if !answered[i] {
			r.records[i] = r.record(i, r.events[i], r.timeout, codeDeadlineExceeded, 0)
		}
	}
}

// cancelUnsent records the calls a canceled run never sent.
func (r *runner) cancelUnsent(ctx context.Context) {
	if ctx.Err() == nil {
		return
	}
	for i, ev := range r.events {
		if r.records[i].Kind == "" {
			r.records[i] = r.record(i, ev, 0, codeCanceled, 0)
		}
	}
}

func (r *runner) record(seq int, ev Event, latency time.Duration, code uint32, resp int) Record {
	req := ev.ReqSize
	if ev.Kind == KindConnect {
		req = 0
	}
	return Record{
		Seq: seq, Kind: ev.Kind, Scheduled: ev.At, Latency: latency, Code: code,
		ReqBytes: req, RespBytes: resp, Conn: ev.Conn, Warmup: ev.At < r.cfg.Warmup,
	}
}

// closed runs the small and heavy calls of the schedule one after another
// on cfg.Closed workers until the run is over, cycling through the schedule
// for their sizes. A call counts from when it is sent.
func (r *runner) closed(ctx context.Context, total time.Duration) ([]Record, error) {
	if r.cfg.Profile.Echo != nil || r.cfg.Profile.Reconnect != nil {
		return nil, errors.New("loadgen: closed-loop takes small and heavy calls only")
	}
	var calls []Event
	for _, ev := range Schedule(r.cfg.Profile, r.cfg.Seed, total) {
		if ev.Kind == KindSmall || ev.Kind == KindHeavy {
			calls = append(calls, ev)
		}
	}
	if len(calls) == 0 {
		return nil, errors.New("loadgen: the schedule has no calls")
	}
	c, err := r.cfg.Dial()
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()
	connectCtx, cancel := context.WithTimeout(ctx, r.timeout)
	_ = c.Connect(connectCtx)
	cancel()

	r.start = time.Now()
	var mu sync.Mutex
	next := 0
	var wg sync.WaitGroup
	for range r.cfg.Closed {
		wg.Go(func() {
			for ctx.Err() == nil {
				sent := time.Since(r.start)
				if sent >= total {
					return
				}
				mu.Lock()
				seq := next
				next++
				mu.Unlock()
				ev := calls[seq%len(calls)]
				ev.At = sent
				rec := r.call(ctx, c, seq, ev)
				mu.Lock()
				r.records = append(r.records, rec)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	slices.SortFunc(r.records, func(a, b Record) int { return cmp.Compare(a.Seq, b.Seq) })
	return r.records, ctx.Err()
}

// payloads backs request payloads; Call copies it into the message.
var payloads = make([]byte, 1<<16)

func payload(n int) []byte {
	if n <= len(payloads) {
		return payloads[:n]
	}
	return make([]byte, n)
}

// sleepUntil waits for t and reports whether ctx is still alive.
func sleepUntil(ctx context.Context, t time.Time) bool {
	d := time.Until(t)
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
