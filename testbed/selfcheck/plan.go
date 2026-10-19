package selfcheck

import (
	"math"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
)

// Tolerance is the allowed deviation of a measurement from its target, as
// a fraction of the target (NET-4). A target of zero must be met exactly.
const Tolerance = 0.1

// sigmas is how many standard deviations of an estimate fit into the
// tolerance: a correct testbed fails a check with probability about 0.3%,
// and a whole self-check with about 1%.
const sigmas = 3

// MinPackets is the smallest probe per direction (NET-4).
const MinPackets = 100000

// MaxPacketRate is the default probe rate cap, packets per second.
const MaxPacketRate = 50000

// probePayload is the UDP payload of a probe packet; with the UDP, IP and
// Ethernet headers it takes probeFrame bytes on the wire.
const (
	probePayload = 64
	probeFrame   = probePayload + 8 + 20 + 14
)

const (
	minPings        = 20
	minPingInterval = 10 * time.Millisecond
	// medianSE is the standard error of the median of n normal samples
	// times √n divided by σ: √(π/2).
	medianSE = 1.2533
)

// Plan is how much a self-check measures for a profile.
type Plan struct {
	// Packets is the probe size per direction.
	Packets int
	// PacketRate is the probe rate per direction, packets per second.
	PacketRate float64
	// Pings is how many ping replies the RTT median needs; PingInterval
	// keeps pings from queueing behind each other under jitter.
	Pings        int
	PingInterval time.Duration
}

// Duration estimates how long the probe takes.
func (pl Plan) Duration() time.Duration {
	return time.Duration(float64(pl.Packets) / pl.PacketRate * float64(time.Second))
}

// NewPlan sizes the measurements of p so that each estimate stays within
// the tolerance with a margin of three standard deviations. maxRate caps
// the probe rate; minPackets is the smallest probe.
func NewPlan(p netns.Profile, maxRate float64, minPackets int) Plan {
	rate := maxRate
	if p.Rate > 0 {
		// Half the bottleneck: the probe never fills its queue.
		rate = min(rate, float64(p.Rate)/8/probeFrame/2)
	}
	pings, interval := pingPlan(p)
	return Plan{Packets: packets(p, minPackets), PacketRate: rate, Pings: pings, PingInterval: interval}
}

// packets returns the probe size per direction. For a share q estimated
// from n independent packets the standard deviation is √(q(1−q)/n), so
// staying within Tolerance·q at sigmas standard deviations takes
// n = (sigmas/Tolerance)²·(1−q)/q.
func packets(p netns.Profile, minPackets int) int {
	const k = sigmas * sigmas / (Tolerance * Tolerance)
	n := float64(minPackets)
	loss := p.Loss.Fraction()
	if loss > 0 {
		inflation := 1.0
		if p.LossBurst > 1 {
			// Losses in bursts are not independent. For the two-state
			// chain of the Gilbert model the variance of the loss share
			// grows by (1+ρ)/(1−ρ), ρ = 1−p−r being its correlation from
			// one packet to the next.
			good2bad, bad2good := p.Gemodel()
			g, b := good2bad.Fraction(), bad2good.Fraction()
			rho := 1 - g - b
			inflation = (1 + rho) / (1 - rho)
			// The mean burst length: about n·loss/LossBurst bursts of
			// geometric length, whose relative standard deviation is √(1−r).
			n = max(n, k*(1-b)*p.LossBurst/loss)
		}
		n = max(n, k*(1-loss)/loss*inflation)
	}
	if r := p.Reorder.Fraction(); r > 0 {
		// The share is taken among the packets that arrive.
		n = max(n, k*(1-r)/r/(1-loss))
	}
	return int(math.Ceil(n))
}

// pingPlan sizes the RTT measurement. The median of n normal samples has a
// standard error of about 1.2533·σ/√n, and the RTT varies with
// σ = √2·jitter, the two directions being independent.
func pingPlan(p netns.Profile) (int, time.Duration) {
	n := minPings
	if p.Jitter > 0 && p.RTT > 0 {
		sigma := math.Sqrt2 * p.Jitter.Seconds()
		root := sigmas * medianSE * sigma / (Tolerance * p.RTT.Seconds())
		n = max(n, int(math.Ceil(root*root)))
	}
	return n, max(minPingInterval, 4*p.Jitter)
}
