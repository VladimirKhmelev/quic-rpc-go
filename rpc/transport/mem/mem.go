// Package mem is an in-process transport for tests and for measuring the RPC
// layer without network and encryption.
//
// It mirrors QUIC stream semantics: every stream direction has a bounded
// buffer (the flow-control window), Close sends FIN, CancelWrite and
// CancelRead reset a direction, the number of concurrently open streams is
// limited and closing the connection fails every stream.
//
// Unlike QUIC there is no connection-level flow-control window, so code that
// deadlocks only when many streams together exhaust it passes on mem; run
// such tests on the quic transport as well.
package mem

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// Defaults match quic-go's initial stream receive window and its default
// limit on incoming streams.
const (
	DefaultStreamWindow       = 512 << 10
	DefaultMaxIncomingStreams = 100
)

// acceptQueueSize matches quic-go's queue of connections waiting for Accept.
const acceptQueueSize = 32

// Options configures a Listener. Zero fields take the defaults.
type Options struct {
	// StreamWindow is how many bytes a stream direction buffers before
	// Write blocks.
	StreamWindow int
	// MaxIncomingStreams limits the streams a connection may have open at
	// the same time.
	MaxIncomingStreams int
}

func (o Options) withDefaults() Options {
	if o.StreamWindow <= 0 {
		o.StreamWindow = DefaultStreamWindow
	}
	if o.MaxIncomingStreams <= 0 {
		o.MaxIncomingStreams = DefaultMaxIncomingStreams
	}
	return o
}

var errServerOpen = errors.New("mem: streams are opened by the client side only")

// Listener accepts in-process connections created by its Dial method, so it
// is both a transport.Listener and a transport.Dialer.
type Listener struct {
	opts      Options
	addr      addr
	queue     chan *conn
	closed    chan struct{}
	closeOnce sync.Once
}

var listenerSeq atomic.Uint64

// Listen returns a new listener.
func Listen(opts Options) *Listener {
	return &Listener{
		opts:   opts.withDefaults(),
		addr:   addr(fmt.Sprintf("mem-%d", listenerSeq.Add(1))),
		queue:  make(chan *conn, acceptQueueSize),
		closed: make(chan struct{}),
	}
}

// Dial connects to the listener. It waits while the accept queue is full.
func (l *Listener) Dial(ctx context.Context) (transport.Conn, error) {
	if isClosed(l.closed) {
		return nil, transport.ErrListenerClosed
	}
	client, server := newConnPair(l.opts)
	select {
	case l.queue <- server:
		return client, nil
	case <-l.closed:
		return nil, transport.ErrListenerClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Accept waits for the next connection. As in quic-go, connections already
// queued are still returned after Close.
func (l *Listener) Accept(ctx context.Context) (transport.Conn, error) {
	select {
	case c := <-l.queue:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closed:
		select {
		case c := <-l.queue:
			return c, nil
		default:
			return nil, transport.ErrListenerClosed
		}
	}
}

// Close stops accepting connections.
func (l *Listener) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

// Addr returns the listener's in-process address.
func (l *Listener) Addr() net.Addr { return l.addr }

type addr string

func (addr) Network() string  { return "mem" }
func (a addr) String() string { return string(a) }

const (
	clientSide = 0
	serverSide = 1
)

// session is the state shared by the two ends of a connection.
type session struct {
	window int
	// slots holds one token per open stream; its capacity is the stream limit.
	slots chan struct{}
	// accept queues streams opened by the client until the server accepts
	// them. A stream keeps its slot until accepted, so this never fills up.
	accept chan *stream
	ends   [2]*conn
	// mu guards released, which is closed and replaced whenever a stream
	// gives its slot back.
	mu       sync.Mutex
	released chan struct{}

	done      chan struct{}
	closeOnce sync.Once
	// Written once before done is closed.
	closedBy int
	code     transport.ErrorCode
	reason   string
}

// errFor returns the error the given side sees after the connection closed.
func (s *session) errFor(side int) error {
	return &transport.ConnError{Code: s.code, Remote: side != s.closedBy, Reason: s.reason}
}

type conn struct {
	s      *session
	side   int
	ctx    context.Context
	cancel context.CancelCauseFunc
}

func newConnPair(opts Options) (client, server *conn) {
	s := &session{
		window:   opts.StreamWindow,
		slots:    make(chan struct{}, opts.MaxIncomingStreams),
		accept:   make(chan *stream, opts.MaxIncomingStreams),
		released: make(chan struct{}),
		done:     make(chan struct{}),
	}
	for side := range s.ends {
		ctx, cancel := context.WithCancelCause(context.Background())
		s.ends[side] = &conn{s: s, side: side, ctx: ctx, cancel: cancel}
	}
	return s.ends[clientSide], s.ends[serverSide]
}

func (c *conn) OpenStream(ctx context.Context) (transport.Stream, error) {
	if c.side != clientSide {
		return nil, errServerOpen
	}
	if isClosed(c.s.done) {
		return nil, c.s.errFor(c.side)
	}
	select {
	case c.s.slots <- struct{}{}:
	case <-c.s.done:
		return nil, c.s.errFor(c.side)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	clientEnd, serverEnd := newStreamPair(c.s, c.s.release)
	c.s.accept <- serverEnd
	return clientEnd, nil
}

func (c *conn) AcceptStream(ctx context.Context) (transport.Stream, error) {
	if isClosed(c.s.done) {
		return nil, c.s.errFor(c.side)
	}
	var incoming chan *stream
	if c.side == serverSide {
		incoming = c.s.accept
	}
	select {
	case st := <-incoming:
		st.pair.step()
		return st, nil
	case <-c.s.done:
		return nil, c.s.errFor(c.side)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// release gives the slot of a finished stream back.
func (s *session) release() {
	<-s.slots
	s.mu.Lock()
	close(s.released)
	s.released = make(chan struct{})
	s.mu.Unlock()
}

// Drain waits until no stream holds a slot: every stream is done in both
// directions, read up to FIN or reset, and accepted. Unlike QUIC, mem knows
// exactly that the peer has read everything.
func (c *conn) Drain(ctx context.Context) error {
	for {
		c.s.mu.Lock()
		released := c.s.released
		c.s.mu.Unlock()
		if len(c.s.slots) == 0 || isClosed(c.s.done) {
			return nil
		}
		select {
		case <-released:
		case <-c.s.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (c *conn) CloseWithError(code transport.ErrorCode, reason string) error {
	c.s.closeOnce.Do(func() {
		c.s.closedBy = c.side
		c.s.code = code
		c.s.reason = reason
		// Contexts first: whatever wakes up on done and reports the
		// connection closed must find them canceled (see transport.Conn).
		for side, end := range c.s.ends {
			end.cancel(c.s.errFor(side))
		}
		close(c.s.done)
	})
	return nil
}

func (c *conn) Context() context.Context { return c.ctx }

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
