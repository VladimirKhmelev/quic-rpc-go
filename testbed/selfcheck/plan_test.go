package selfcheck

import (
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
)

var degraded = netns.Profile{
	Name: "degraded", RTT: 100 * time.Millisecond, Jitter: 20 * time.Millisecond,
	Loss: 2, LossBurst: 4, Reorder: 1, Rate: 10e6, QueueBDP: 1,
}

func TestPackets(t *testing.T) {
	tests := []struct {
		name string
		p    netns.Profile
		want int
	}{
		{"no impairments", netns.None, MinPackets},
		{"loss 0.1%", netns.Profile{Loss: 0.1}, 899100},
		{"loss 0.5%", netns.Profile{Loss: 0.5}, 179100},
		{"loss 1% is below the floor", netns.Profile{Loss: 1}, MinPackets},
		{"reorder 0.5%", netns.Profile{RTT: time.Millisecond, Reorder: 0.5}, 179100},
		{"reorder among the arrived", netns.Profile{RTT: time.Millisecond, Loss: 10, Reorder: 0.5}, 199000},
		// 900·0.98/0.02 for the loss share, times (1+ρ)/(1−ρ) = 6.84 for
		// bursts of four.
		{"loss in bursts", degraded, 301644},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Float rounding may add one packet.
			if got := packets(tt.p, MinPackets); got < tt.want || got > tt.want+1 {
				t.Fatalf("packets = %d, want %d", got, tt.want)
			}
		})
	}
	if got := packets(netns.None, 1000); got != 1000 {
		t.Fatalf("packets with a floor of 1000 = %d", got)
	}
}

func TestNewPlan(t *testing.T) {
	pl := NewPlan(degraded, MaxPacketRate, MinPackets)
	// Half of 10 Mbit/s in frames of 106 bytes.
	if want := 10e6 / 8 / 106 / 2; pl.PacketRate != want {
		t.Errorf("rate %v, want %v", pl.PacketRate, want)
	}
	// (3·1.2533·√2·20 ms / 10 ms)² = 113.1.
	if pl.Pings != 114 || pl.PingInterval != 80*time.Millisecond {
		t.Errorf("%d pings every %v, want 114 every 80ms", pl.Pings, pl.PingInterval)
	}
	if got := pl.Duration().Round(time.Second); got != 51*time.Second {
		t.Errorf("duration %v, want 51s", got)
	}
	unlimited := NewPlan(netns.Profile{RTT: time.Millisecond}, MaxPacketRate, MinPackets)
	if unlimited.PacketRate != MaxPacketRate || unlimited.Pings != minPings || unlimited.PingInterval != minPingInterval {
		t.Errorf("plan without rate and jitter: %+v", unlimited)
	}
}
