package selfcheck

import (
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
)

func TestAnalyze(t *testing.T) {
	tests := []struct {
		name     string
		sent     int
		arrivals []uint32
		want     lossStats
	}{
		{"all in order", 4, []uint32{0, 1, 2, 3}, lossStats{Sent: 4, Received: 4}},
		{"nothing arrived", 3, nil, lossStats{Sent: 3, Lost: 3, Bursts: 1}},
		{"two bursts", 10, []uint32{0, 1, 2, 6, 7, 8},
			lossStats{Sent: 10, Received: 6, Lost: 4, Bursts: 2}},
		{"first and last lost", 5, []uint32{1, 2, 3}, lossStats{Sent: 5, Received: 3, Lost: 2, Bursts: 2}},
		// 5 overtook 2, 3 and 4: one packet out of place, not three.
		{"one packet overtakes several", 7, []uint32{0, 1, 5, 2, 3, 4, 6},
			lossStats{Sent: 7, Received: 7, Reordered: 1}},
		{"two overtake", 8, []uint32{3, 0, 1, 2, 7, 4, 5, 6},
			lossStats{Sent: 8, Received: 8, Reordered: 2}},
		{"a late packet", 5, []uint32{0, 2, 3, 4, 1}, lossStats{Sent: 5, Received: 5, Reordered: 1}},
		// One place where a number drops, but two packets out of place.
		{"blocks swapped", 4, []uint32{2, 3, 0, 1}, lossStats{Sent: 4, Received: 4, Reordered: 2}},
		{"duplicates", 3, []uint32{0, 1, 1, 2, 2}, lossStats{Sent: 3, Received: 3, Duplicates: 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := analyze(tt.sent, tt.arrivals); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("analyze = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestLongestIncreasing compares with an O(n²) dynamic program on random
// permutations.
func TestLongestIncreasing(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for range 500 {
		seqs := make([]uint32, rng.IntN(30))
		for i, v := range rng.Perm(len(seqs)) {
			seqs[i] = uint32(v) //nolint:gosec // below 30
		}
		want := 0
		best := make([]int, len(seqs))
		for i := range seqs {
			best[i] = 1
			for j := range i {
				if seqs[j] < seqs[i] {
					best[i] = max(best[i], best[j]+1)
				}
			}
			want = max(want, best[i])
		}
		if got := longestIncreasing(seqs); got != want {
			t.Fatalf("longestIncreasing(%v) = %d, want %d", seqs, got, want)
		}
	}
}

func TestWithin(t *testing.T) {
	tests := []struct {
		measured, target float64
		want             bool
	}{
		{1.099, 1, true}, {0.901, 1, true}, {1.101, 1, false}, {0.899, 1, false},
		{0, 0, true}, {0.001, 0, false},
	}
	for _, tt := range tests {
		if got := within(tt.measured, tt.target); got != tt.want {
			t.Errorf("within(%v, %v) = %v", tt.measured, tt.target, got)
		}
	}
	if got := median([]time.Duration{5, 1, 3}); got != 3 {
		t.Errorf("median = %v", got)
	}
}

// missRates simulates trials of n packets losing packets as netem does and
// returns how often the loss share and the mean burst length missed the
// tolerance.
func missRates(seed uint64, trials, n int, p netns.Profile) (loss, burst float64) {
	q := p.Loss.Fraction()
	good2bad, bad2good := p.Gemodel()
	g, b := good2bad.Fraction(), bad2good.Fraction()
	rng := rand.New(rand.NewPCG(seed, 1))
	var lossMisses, burstMisses int
	for range trials {
		lost, bursts := 0, 0
		bad, prev := rng.Float64() < q, false
		for range n {
			var isLost bool
			if p.LossBurst > 1 {
				// A packet that finds the chain in the bad state is lost,
				// then the chain moves on (sch_netem.c, loss_gilb_ell).
				isLost = bad
				if bad {
					bad = rng.Float64() >= b
				} else {
					bad = rng.Float64() < g
				}
			} else {
				isLost = rng.Float64() < q
			}
			if isLost {
				lost++
				if !prev {
					bursts++
				}
			}
			prev = isLost
		}
		if !within(float64(lost)/float64(n), q) {
			lossMisses++
		}
		if p.LossBurst > 1 && (bursts == 0 || !within(float64(lost)/float64(bursts), p.LossBurst)) {
			burstMisses++
		}
	}
	return float64(lossMisses) / float64(trials), float64(burstMisses) / float64(trials)
}

// TestPacketsKeepFalseFailuresRare checks the sizing of the probe against a
// simulation: with packets(p) a correct testbed misses the tolerance in
// about 0.3% of the checks, and with too few packets far more often, so the
// simulation would expose a wrong formula.
func TestPacketsKeepFalseFailuresRare(t *testing.T) {
	independent := netns.Profile{Loss: 0.5}
	tests := []struct {
		name             string
		p                netns.Profile
		n, trials        int
		minMiss, maxMiss float64 // bounds on how often the loss share misses
	}{
		{"independent", independent, packets(independent, 1), 300, 0, 0.01},
		// Half the standard deviations: about 13% misses.
		{"independent, a quarter of the packets", independent, packets(independent, 1) / 4, 300, 0.05, 1},
		{"bursts", degraded, packets(degraded, 1), 200, 0, 0.01},
		// Sized as if losses were independent, (1+ρ)/(1−ρ) = 6.84 times too
		// few: about 25% misses.
		{"bursts sized as independent", degraded, 900 * 98 / 2, 200, 0.1, 1},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loss, burst := missRates(uint64(i)+1, tt.trials, tt.n, tt.p) //nolint:gosec // a small index
			if loss < tt.minMiss || loss > tt.maxMiss {
				t.Errorf("%d packets: the loss share missed in %.1f%% of trials, want %.0f%%…%.0f%%",
					tt.n, 100*loss, 100*tt.minMiss, 100*tt.maxMiss)
			}
			if tt.p.LossBurst > 1 && tt.maxMiss < 1 && burst > tt.maxMiss {
				t.Errorf("%d packets: the burst length missed in %.1f%% of trials", tt.n, 100*burst)
			}
		})
	}
}
