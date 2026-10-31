package transporttest

import (
	"net"
	"sync"
	"testing"
	"time"
)

// PacketConn queues outgoing datagrams with an independent delay per packet.
// Incoming packets are untouched, so Delay is the added round-trip time. It
// can also hold writes to keep a real QUIC handshake from finishing in tests.
type PacketConn struct {
	net.PacketConn
	Delay   time.Duration // set before use
	mu      sync.Mutex
	gate    *packetGate
	queue   chan queuedPacket
	done    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

type packetGate struct {
	ready   chan struct{}
	release chan struct{}
	once    sync.Once
}

type queuedPacket struct {
	data []byte
	addr net.Addr
	at   time.Time
	gate *packetGate
}

// NewPacketConn opens a loopback UDP socket and closes it during cleanup.
func NewPacketConn(t testing.TB) *PacketConn {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &PacketConn{PacketConn: pc, queue: make(chan queuedPacket, 256), done: make(chan struct{}), stopped: make(chan struct{})}
	go c.run()
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// HoldWrites holds new packets until release is called. ready closes when a
// packet reaches the gate. release is safe to call more than once.
func (c *PacketConn) HoldWrites() (ready <-chan struct{}, release func()) {
	g := &packetGate{ready: make(chan struct{}), release: make(chan struct{})}
	c.mu.Lock()
	c.gate = g
	c.mu.Unlock()
	var once sync.Once
	return g.ready, func() {
		once.Do(func() {
			c.mu.Lock()
			c.gate = nil
			close(g.release)
			c.mu.Unlock()
		})
	}
}

// WriteTo copies a packet into the queue; run applies its gate and delay.
func (c *PacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	g := c.gate
	c.mu.Unlock()
	packet := queuedPacket{data: append([]byte(nil), p...), addr: addr, at: time.Now().Add(c.Delay), gate: g}
	select {
	case <-c.done:
		return 0, net.ErrClosed
	case c.queue <- packet:
		return len(p), nil
	}
}

func (c *PacketConn) run() {
	defer close(c.stopped)
	for {
		select {
		case <-c.done:
			return
		case p := <-c.queue:
			if p.gate != nil {
				p.gate.once.Do(func() { close(p.gate.ready) })
				select {
				case <-p.gate.release:
				case <-c.done:
					return
				}
			}
			if delay := time.Until(p.at); delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-timer.C:
				case <-c.done:
					timer.Stop()
					return
				}
			}
			_, _ = c.PacketConn.WriteTo(p.data, p.addr)
		}
	}
}

// Close closes the socket and unblocks the queued writer, including its gate.
func (c *PacketConn) Close() error {
	c.once.Do(func() { close(c.done) })
	err := c.PacketConn.Close()
	<-c.stopped
	return err
}
