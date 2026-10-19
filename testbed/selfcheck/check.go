//go:build linux

package selfcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
)

// Options tune a self-check; the zero value gives the defaults.
type Options struct {
	// MaxPacketRate caps the probe rate per direction, packets per second;
	// zero means MaxPacketRate.
	MaxPacketRate float64
	// MinPackets is the smallest probe; zero means MinPackets. Only tests
	// should go below it.
	MinPackets int
	// Log, if set, receives progress messages.
	Log io.Writer

	rcvbuf int // the probe's socket buffer, for tests
}

// Run measures the running testbed against profile p (NET-4): the RTT with
// ping, losses and reordering with a probe of numbered UDP packets in both
// directions at once, and the bandwidth with iperf3. The testbed must have
// been built with the impairments of p.
func Run(ctx context.Context, p netns.Profile, opts Options) (*Report, error) {
	if opts.MaxPacketRate == 0 {
		opts.MaxPacketRate = MaxPacketRate
	}
	if opts.MinPackets == 0 {
		opts.MinPackets = MinPackets
	}
	logf := func(format string, args ...any) {
		if opts.Log != nil {
			_, _ = fmt.Fprintf(opts.Log, format+"\n", args...)
		}
	}
	plan := NewPlan(p, opts.MaxPacketRate, opts.MinPackets)
	r := &Report{Profile: p.Name, Plan: plan}

	if p.RTT > 0 {
		logf("%s: %d pings every %v", p.Name, plan.Pings, plan.PingInterval)
		if err := checkRTT(ctx, p, plan, r); err != nil {
			return nil, err
		}
	}
	logf("%s: %d packets each way at %.0f packets/s, about %v", p.Name, plan.Packets, plan.PacketRate, plan.Duration().Round(time.Second))
	if err := checkLoss(ctx, p, plan, opts, r); err != nil {
		return nil, err
	}
	if p.Rate > 0 {
		logf("%s: iperf3 for %d s each way", p.Name, iperfSeconds)
		if err := checkBandwidth(ctx, p, r); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func checkRTT(ctx context.Context, p netns.Profile, plan Plan, r *Report) error {
	// Enough pings that plan.Pings replies come back despite the losses
	// in both directions.
	keep := (1 - p.Loss.Fraction()) * (1 - p.Loss.Fraction())
	count := int(math.Ceil(float64(plan.Pings)/keep*1.1)) + 5
	timeout := int(math.Ceil((3*p.RTT + time.Second).Seconds()))
	out, err := netns.Command(ctx, netns.Client, "ping", "-n", "-c", strconv.Itoa(count),
		"-i", strconv.FormatFloat(plan.PingInterval.Seconds(), 'f', -1, 64),
		"-W", strconv.Itoa(timeout), netns.ServerAddr.String()).CombinedOutput()
	rtts, perr := parsePing(string(out))
	if perr != nil || len(rtts) == 0 {
		// ping fails only without replies; with some lost it exits 1 too.
		return fmt.Errorf("ping: %w: %s", errors.Join(err, perr), out)
	}
	if len(rtts) < plan.Pings {
		r.invalid("%d ping replies, %d needed", len(rtts), plan.Pings)
	}
	r.add(Check{Name: "rtt", Unit: "ms", Target: millis(p.RTT), Measured: millis(median(rtts)), Samples: len(rtts)})
	return nil
}

func checkLoss(ctx context.Context, p netns.Profile, plan Plan, opts Options, r *Report) error {
	cfg := probeConfig{
		packets: plan.Packets,
		rate:    plan.PacketRate,
		// A late packet waits for the one-way delay, the jitter and a full
		// bottleneck queue.
		drain:  p.RTT/2 + 4*p.Jitter + queueDelay(p) + 200*time.Millisecond,
		rcvbuf: opts.rcvbuf,
	}
	before, err := readCounters(ctx)
	if err != nil {
		return err
	}
	arrivals := make([][]uint32, len(directions))
	errs := make([]error, len(directions))
	var wg sync.WaitGroup
	for i, d := range directions {
		wg.Go(func() { arrivals[i], errs[i] = probe(ctx, d, cfg) })
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	after, err := readCounters(ctx)
	if err != nil {
		return err
	}
	after.explainDrops(before, r)

	for i, d := range directions {
		s := analyze(plan.Packets, arrivals[i])
		if s.Duplicates > 0 {
			r.invalid("%s: %d duplicated packets", d.Name, s.Duplicates)
		}
		r.add(Check{Name: "loss", Direction: d.Name, Unit: "%", Target: float64(p.Loss),
			Measured: 100 * float64(s.Lost) / float64(s.Sent), Samples: s.Sent})
		if p.LossBurst > 1 {
			burst := 0.0
			if s.Bursts > 0 {
				burst = float64(s.Lost) / float64(s.Bursts)
			}
			r.add(Check{Name: "loss burst", Direction: d.Name, Unit: "packets", Target: p.LossBurst,
				Measured: burst, Samples: s.Bursts})
		}
		reordered := 0.0
		if s.Received > 0 {
			reordered = 100 * float64(s.Reordered) / float64(s.Received)
		}
		r.add(Check{Name: "reorder", Direction: d.Name, Unit: "%", Target: float64(p.Reorder),
			Measured: reordered, Samples: s.Received})
	}
	return nil
}

// iperf3 sends datagrams of iperfPayload bytes for iperfSeconds and offers
// 125% of the bottleneck rate: more than it passes even after the losses
// of netem, which sits before it.
const (
	iperfPayload = 1400
	iperfSeconds = 5
	// iperfFrame is the size on the wire: the bottleneck counts the UDP, IP
	// and Ethernet headers too, iperf3 only the payload.
	iperfFrame = iperfPayload + 8 + 20 + 14
)

// checkBandwidth measures one direction at a time: the control connection
// of iperf3 runs both ways, and a flood in the opposite direction can stall
// it and spoil the test.
func checkBandwidth(ctx context.Context, p netns.Profile, r *Report) error {
	want := float64(p.Rate) * iperfPayload / iperfFrame
	for _, d := range directions {
		bps, intervals, err := goodput(ctx, d, p.Rate)
		if err != nil {
			return err
		}
		r.add(Check{Name: "bandwidth", Direction: d.Name, Unit: "Mbit/s", Target: want / 1e6, Measured: bps / 1e6, Samples: intervals})
	}
	return nil
}

// iperfAttempts is how many times a failed iperf3 run is repeated. A
// failure is not a measurement: iperf3 sets up its UDP test with single
// datagrams, which the impairments may lose, and then waits long for the
// reply; iperfAttemptTimeout cuts that wait short.
const (
	iperfAttempts       = 3
	iperfAttemptTimeout = (iperfSeconds + 10) * time.Second
)

// goodput measures the UDP goodput of direction d with iperf3.
func goodput(ctx context.Context, d direction, rate netns.Rate) (bps float64, intervals int, err error) {
	var errs []error
	for range iperfAttempts {
		bps, intervals, err := iperfRun(ctx, d, rate)
		if err == nil {
			return bps, intervals, nil
		}
		errs = append(errs, err)
	}
	return 0, 0, fmt.Errorf("iperf3 %s: %w", d.Name, errors.Join(errs...))
}

func iperfRun(ctx context.Context, d direction, rate netns.Rate) (bps float64, intervals int, err error) {
	port := strconv.Itoa(int(d.Port) + 100)
	var srvOut bytes.Buffer
	srv := netns.Command(ctx, d.To, "iperf3", "-s", "-1", "-J", "-B", d.Dst.String(), "-p", port)
	srv.Stdout, srv.Stderr = &srvOut, &srvOut
	if err := srv.Start(); err != nil {
		return 0, 0, fmt.Errorf("server: %w", err)
	}
	// The server output may be read only after Wait, which ends the
	// goroutine that copies it.
	stopServer := func() string {
		_ = srv.Process.Kill()
		_ = srv.Wait()
		return srvOut.String()
	}
	offered := strconv.FormatUint(uint64(rate)/4*5, 10)
	for wait := 0; ; wait++ {
		attempt, cancel := context.WithTimeout(ctx, iperfAttemptTimeout)
		out, err := netns.Command(attempt, d.From, "iperf3", "-c", d.Dst.String(), "-p", port, "-u",
			"-b", offered, "-l", strconv.Itoa(iperfPayload), "-t", strconv.Itoa(iperfSeconds), "-J", "--get-server-output").Output()
		cancel()
		bps, intervals, perr := parseIperf(out)
		switch {
		case perr == nil:
			stopServer()
			return bps, intervals, nil
		case errors.Is(perr, errNotListening) && wait < 30:
			time.Sleep(100 * time.Millisecond)
		default:
			return 0, 0, fmt.Errorf("%w (server: %s)", errors.Join(err, perr), stopServer())
		}
	}
}

func queueDelay(p netns.Profile) time.Duration {
	if p.Rate == 0 {
		return 0
	}
	return time.Duration(float64(p.QueueBytes()) * 8 / float64(p.Rate) * float64(time.Second))
}

func millis(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// counters are the drop counters that make a probe invalid (see
// counters.go), by receiver namespace and by router interface.
type counters struct {
	rcvbuf  map[string]uint64
	tbf     map[string]uint64
	dev     map[string]uint64 // interface drops by namespace
	softnet uint64
}

func readCounters(ctx context.Context) (counters, error) {
	c := counters{rcvbuf: map[string]uint64{}, tbf: map[string]uint64{}, dev: map[string]uint64{}}
	for _, ns := range []string{netns.Client, netns.Router, netns.Server} {
		err := netns.Do(ns, func() error {
			dev, err := os.ReadFile("/proc/thread-self/net/dev")
			if err != nil {
				return err
			}
			c.dev[ns], err = parseDevDrops(string(dev))
			return err
		})
		if err != nil {
			return c, err
		}
	}
	for _, d := range directions {
		err := netns.Do(d.To, func() error {
			snmp, err := os.ReadFile("/proc/thread-self/net/snmp")
			if err != nil {
				return err
			}
			c.rcvbuf[d.To], err = parseUDPRcvbufErrors(string(snmp))
			return err
		})
		if err != nil {
			return c, err
		}
		out, err := netns.Command(ctx, netns.Router, "tc", "-s", "qdisc", "show", "dev", d.Out).Output()
		if err != nil {
			return c, fmt.Errorf("tc -s qdisc show dev %s: %w", d.Out, err)
		}
		if c.tbf[d.Out], err = parseTbfDropped(string(out)); err != nil {
			return c, err
		}
	}
	stat, err := os.ReadFile("/proc/net/softnet_stat")
	if err != nil {
		return c, err
	}
	c.softnet, err = parseSoftnetDropped(string(stat))
	return c, err
}

func (c counters) explainDrops(before counters, r *Report) {
	for _, d := range directions {
		if n := c.rcvbuf[d.To] - before.rcvbuf[d.To]; n > 0 {
			r.invalid("%s: the probe read too slowly, %d packets overflowed its socket buffer (RcvbufErrors)", d.Name, n)
		}
		if n := c.tbf[d.Out] - before.tbf[d.Out]; n > 0 {
			r.invalid("%s: the probe overran the bottleneck, tbf dropped %d packets", d.Name, n)
		}
	}
	for _, ns := range []string{netns.Client, netns.Router, netns.Server} {
		if n := c.dev[ns] - before.dev[ns]; n > 0 {
			r.invalid("%s: interfaces dropped %d packets, a receive ring was full (/proc/net/dev)", ns, n)
		}
	}
	if n := c.softnet - before.softnet; n > 0 {
		r.invalid("%d packets dropped from full CPU backlogs (softnet_stat)", n)
	}
}
