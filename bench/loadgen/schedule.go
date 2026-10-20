package loadgen

import (
	"cmp"
	"math/rand/v2"
	"slices"
	"time"
)

// Kind is what a scheduled event does.
type Kind string

// The kinds of events.
const (
	KindSmall   Kind = "small"
	KindHeavy   Kind = "heavy"
	KindEcho    Kind = "echo"
	KindConnect Kind = "connect"
)

// Event is one scheduled call, message or connection.
type Event struct {
	// At is the time since the start of the run when it is due.
	At       time.Duration
	Kind     Kind
	ReqSize  int
	RespSize int
	// Conn is the connection: 0 is the one of the whole run, n > 0 the
	// n-th of a reconnect profile.
	Conn int
}

// Streams of random numbers, one per part of a profile, so that adding a
// part leaves the others as they were: small calls are the same with and
// without a heavy one.
const (
	streamSmall uint64 = iota + 1
	streamEcho
	streamReconnect
)

// Schedule returns the events of p due before total, ordered by time. The
// seed determines them all (GEN-5): the same seed gives the same events.
func Schedule(p Profile, seed uint64, total time.Duration) []Event {
	var events []Event
	poisson := func(stream uint64, rate float64, ev func(rng *rand.Rand, at time.Duration) Event) {
		rng := rand.New(rand.NewPCG(seed, stream)) //nolint:gosec // reproducible, not secret
		for at := time.Duration(0); ; {
			at += time.Duration(rng.ExpFloat64() / rate * float64(time.Second))
			if at >= total {
				return
			}
			events = append(events, ev(rng, at))
		}
	}
	if s := p.Small; s != nil && s.Rate > 0 {
		poisson(streamSmall, s.Rate, func(rng *rand.Rand, at time.Duration) Event {
			n := drawSize(rng, s)
			return Event{At: at, Kind: KindSmall, ReqSize: n, RespSize: n}
		})
	}
	if h := p.Heavy; h != nil {
		for at := h.Every; at < total; at += h.Every {
			events = append(events, Event{At: at, Kind: KindHeavy, RespSize: h.Size})
		}
	}
	if e := p.Echo; e != nil {
		poisson(streamEcho, e.Rate, func(_ *rand.Rand, at time.Duration) Event {
			return Event{At: at, Kind: KindEcho, ReqSize: e.Size, RespSize: e.Size}
		})
	}
	if r := p.Reconnect; r != nil {
		rng := rand.New(rand.NewPCG(seed, streamReconnect)) //nolint:gosec // reproducible, not secret
		for conn, at := 1, time.Duration(0); at < total; conn, at = conn+1, at+r.Every {
			events = append(events, Event{At: at, Kind: KindConnect, Conn: conn})
			for range r.Calls {
				n := drawSize(rng, p.Small)
				events = append(events, Event{At: at, Kind: KindSmall, ReqSize: n, RespSize: n, Conn: conn})
			}
		}
	}
	// Stable: the calls of a reconnection stay after its connect event.
	slices.SortStableFunc(events, func(a, b Event) int { return cmp.Compare(a.At, b.At) })
	return events
}

func drawSize(rng *rand.Rand, s *Small) int { return s.MinSize + rng.IntN(s.MaxSize-s.MinSize+1) }
