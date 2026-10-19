//go:build linux

package selfcheck

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/testbed/netns"
)

// A probe packet carries a run identifier, so that stray datagrams are
// ignored, and its sequence number; the rest is padding.
const (
	runOffset = 0
	seqOffset = 4
)

// readBuffer is the receive buffer the probe asks for. The kernel caps it
// at net.core.rmem_max; drops past it show in RcvbufErrors.
const readBuffer = 8 << 20

// direction is one way through the testbed.
type direction struct {
	netns.Path
	From, To string     // namespaces
	Dst      netip.Addr // address of To
	Port     uint16
}

var directions = []direction{
	{Path: netns.Paths[0], From: netns.Client, To: netns.Server, Dst: netns.ServerAddr, Port: 9001},
	{Path: netns.Paths[1], From: netns.Server, To: netns.Client, Dst: netns.ClientAddr, Port: 9002},
}

// probeConfig is one probe run.
type probeConfig struct {
	packets int
	rate    float64 // packets per second
	// drain is how long the receiver waits for another packet once the
	// last one is sent.
	drain time.Duration
	// rcvbuf overrides readBuffer in tests.
	rcvbuf int
	// readDelay slows the receiver down in tests.
	readDelay time.Duration
}

// probe sends cfg.packets numbered datagrams through d at cfg.rate and
// returns the sequence numbers in the order they arrived.
func probe(ctx context.Context, d direction, cfg probeConfig) ([]uint32, error) {
	var rx, tx *net.UDPConn
	err := netns.Do(d.To, func() (err error) {
		rx, err = net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(d.Dst, d.Port)))
		return err
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = rx.Close() }()
	if err := rx.SetReadBuffer(orDefault(cfg.rcvbuf, readBuffer)); err != nil {
		return nil, err
	}
	err = netns.Do(d.From, func() (err error) {
		tx, err = net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(d.Dst, d.Port)))
		return err
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Close() }()

	run := rand.Uint32() //nolint:gosec // tells runs apart, not a secret
	type result struct {
		arrivals []uint32
		err      error
	}
	received := make(chan result, 1)
	sent := make(chan struct{})
	go func() {
		arrivals, err := receive(rx, run, cfg, sent)
		received <- result{arrivals, err}
	}()
	sendErr := send(ctx, tx, run, cfg.packets, cfg.rate)
	close(sent)
	// Wakes a receiver that waits for a packet that will not come.
	if err := rx.SetReadDeadline(time.Now().Add(cfg.drain)); err != nil {
		return nil, err
	}
	r := <-received
	if err := errors.Join(sendErr, r.err); err != nil {
		return nil, fmt.Errorf("probe %s: %w", d.Name, err)
	}
	return r.arrivals, nil
}

func orDefault(override, def int) int {
	if override > 0 {
		return override
	}
	return def
}

// send paces the packets: every millisecond it sends those due by then.
func send(ctx context.Context, conn *net.UDPConn, run uint32, n int, rate float64) error {
	buf := make([]byte, probePayload)
	binary.BigEndian.PutUint32(buf[runOffset:], run)
	start := time.Now()
	for i := 0; i < n; {
		due := min(n, int(time.Since(start).Seconds()*rate)+1)
		for ; i < due; i++ {
			binary.BigEndian.PutUint32(buf[seqOffset:], uint32(i)) //nolint:gosec // i < n, an int
			if _, err := conn.Write(buf); err != nil {
				return err
			}
		}
		if i < n {
			if err := ctx.Err(); err != nil {
				return err
			}
			time.Sleep(time.Millisecond)
		}
	}
	return nil
}

// receive reads the probe until no packet has come for cfg.drain after sent
// is closed. The deadline moves with every packet: a receiver that lags
// behind still reads the packets waiting in its socket, while a fixed
// deadline would cut them off and count them as lost.
func receive(conn *net.UDPConn, run uint32, cfg probeConfig, sent <-chan struct{}) ([]uint32, error) {
	n := cfg.packets
	arrivals := make([]uint32, 0, n)
	buf := make([]byte, probePayload+1)
	for {
		select {
		case <-sent:
			if err := conn.SetReadDeadline(time.Now().Add(cfg.drain)); err != nil {
				return arrivals, err
			}
		default:
		}
		if cfg.readDelay > 0 {
			time.Sleep(cfg.readDelay)
		}
		size, err := conn.Read(buf)
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return arrivals, nil
		}
		if err != nil {
			return arrivals, err
		}
		if size != probePayload || binary.BigEndian.Uint32(buf[runOffset:]) != run {
			continue
		}
		if seq := binary.BigEndian.Uint32(buf[seqOffset:]); int(seq) < n {
			arrivals = append(arrivals, seq)
		}
	}
}
