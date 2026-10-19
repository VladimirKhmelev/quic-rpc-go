package selfcheck

import (
	"math"
	"slices"
	"sort"
	"time"
)

// lossStats summarizes one direction of a probe.
type lossStats struct {
	Sent, Received, Duplicates int
	// Lost packets form Bursts runs of consecutive sequence numbers.
	Lost, Bursts int
	// Reordered is how many received packets must be removed for the rest
	// to arrive in order: a packet that overtakes others counts once, not
	// once per packet it overtakes.
	Reordered int
}

// analyze takes the sequence numbers below sent in the order they arrived.
func analyze(sent int, arrivals []uint32) lossStats {
	s := lossStats{Sent: sent}
	seen := make([]bool, sent)
	unique := make([]uint32, 0, len(arrivals))
	for _, seq := range arrivals {
		if seen[seq] {
			s.Duplicates++
			continue
		}
		seen[seq] = true
		unique = append(unique, seq)
	}
	s.Received = len(unique)
	for i, ok := range seen {
		if !ok {
			s.Lost++
			if i == 0 || seen[i-1] {
				s.Bursts++
			}
		}
	}
	s.Reordered = s.Received - longestIncreasing(unique)
	return s
}

// longestIncreasing returns the length of the longest increasing
// subsequence of distinct numbers, in O(n log n).
func longestIncreasing(seqs []uint32) int {
	// tails[i] is the smallest last element of an increasing subsequence
	// of length i+1.
	var tails []uint32
	for _, v := range seqs {
		i := sort.Search(len(tails), func(i int) bool { return tails[i] >= v })
		if i == len(tails) {
			tails = append(tails, v)
		} else {
			tails[i] = v
		}
	}
	return len(tails)
}

// within reports whether measured is within the tolerance of target; a
// target of zero must be met exactly.
func within(measured, target float64) bool {
	if target == 0 {
		return measured == 0
	}
	return math.Abs(measured-target) <= Tolerance*target
}

func median(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[len(s)/2]
}
