package scenarios_test

import (
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
	"github.com/VladimirKhmelev/quic-rpc-go/testbed/scenarios"
)

// TestPoints: the points of block 1 are the base point and one impairment
// at a time around it (EXP-2), with the bottleneck queue of the base point
// but no less than 10 packets.
func TestPoints(t *testing.T) {
	if _, err := netns.ParseProfiles(scenarios.Profiles, scenarios.Points); err != nil {
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
	// Jitter 30ms waits for the pilot, see points.yaml.
	if len(points) != 17 {
		t.Errorf("%d points, want the 18 of block 1 but jitter 30ms", len(points))
	}
	const packet, minPackets = 1500, 10
	for _, p := range points {
		if p.Name == base.Name {
			continue
		}
		impairments := 0
		for _, differs := range []bool{p.RTT != base.RTT, p.Jitter != base.Jitter, p.Loss != base.Loss, p.Reorder != base.Reorder, p.Rate != base.Rate} {
			if differs {
				impairments++
			}
		}
		if impairments != 1 || (p.LossBurst != 0 && p.LossBurst != 4) || p.Outage != (netns.Outage{}) {
			t.Errorf("point %s is not one impairment around the base point: %+v", p.Name, p)
		}
		want := max(float64(p.Rate)/8*base.RTT.Seconds(), packet*minPackets)
		if got := float64(p.QueueBytes()); got < want-1 || got > want+1 {
			t.Errorf("point %s has a queue of %v bytes, want %v", p.Name, got, want)
		}
	}
}
