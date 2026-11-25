package scenarios_test

import (
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/scenarios"
)

// TestPoints checks one impairment at a time around the common base or the
// 150ms jitter reference (EXP-2), with the queue of base and at least 10 packets.
func TestPoints(t *testing.T) {
	if _, err := netns.ParseProfiles(scenarios.Profiles, scenarios.Points, scenarios.LossRTTGrid); err != nil {
		t.Fatal(err)
	}
	points, err := netns.ParseProfiles(scenarios.Points)
	if err != nil {
		t.Fatal(err)
	}
	base, err := netns.Lookup(points, "base")
	if err != nil {
		t.Fatal(err)
	}
	if base.RTT != 50*time.Millisecond || base.Jitter != 0 || base.Loss != 0 || base.Reorder != 0 || base.Rate != 100_000_000 || base.QueueBDP != 1 {
		t.Errorf("base point %+v", base)
	}
	if len(points) != 19 {
		t.Errorf("%d points, want 19 for block 1", len(points))
	}
	axis, err := netns.Lookup(points, "rtt-150")
	if err != nil {
		t.Fatal(err)
	}
	jitter := map[string]time.Duration{"rtt-150-jitter-10": 10 * time.Millisecond, "rtt-150-jitter-30": 30 * time.Millisecond}
	for name, want := range jitter {
		p, err := netns.Lookup(points, name)
		if err != nil {
			t.Fatal(err)
		}
		if p.Jitter != want {
			t.Errorf("%s jitter = %v, want %v", name, p.Jitter, want)
		}
	}
	const packet, minPackets = 1500, 10
	for _, p := range points {
		if p.Name == base.Name {
			continue
		}
		ref := base
		if _, ok := jitter[p.Name]; ok {
			ref = axis
		}
		impairments := 0
		for _, differs := range []bool{p.RTT != ref.RTT, p.Jitter != ref.Jitter, p.Loss != ref.Loss, p.Reorder != ref.Reorder, p.Rate != ref.Rate} {
			if differs {
				impairments++
			}
		}
		if impairments != 1 || (p.LossBurst != 0 && p.LossBurst != 4) || p.Outage != (netns.Outage{}) {
			t.Errorf("point %s is not one impairment around %s: %+v", p.Name, ref.Name, p)
		}
		want := max(float64(p.Rate)/8*base.RTT.Seconds(), packet*minPackets)
		if got := float64(p.QueueBytes()); got < want-1 || got > want+1 {
			t.Errorf("point %s has a queue of %v bytes, want %v", p.Name, got, want)
		}
	}
}

func TestLossRTTGrid(t *testing.T) {
	points, err := netns.ParseProfiles(scenarios.Points, scenarios.LossRTTGrid)
	if err != nil {
		t.Fatal(err)
	}
	rtts := []time.Duration{20 * time.Millisecond, 50 * time.Millisecond, 150 * time.Millisecond}
	losses := []netns.Percent{0, 0.5, 1, 2, 5}
	for _, rtt := range rtts {
		for _, loss := range losses {
			count := 0
			for _, p := range points {
				if p.RTT == rtt && p.Loss == loss && p.LossBurst == 0 && p.Jitter == 0 && p.Reorder == 0 && p.Rate == 100_000_000 && p.Outage == (netns.Outage{}) {
					count++
					if got := p.QueueBytes(); got < 624_999 || got > 625_001 {
						t.Errorf("%s queue = %d bytes, want 625000 ± 1", p.Name, got)
					}
				}
			}
			if count != 1 {
				t.Errorf("RTT %v, loss %v%%: %d profiles, want one", rtt, loss, count)
			}
		}
	}
}
