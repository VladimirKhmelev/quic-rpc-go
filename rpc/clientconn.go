package rpc

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/call"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// errClientConnClosing is returned by calls on a closed ClientConn, as in grpc-go.
var errClientConnClosing = status.Error(codes.Canceled, "client connection is closing")

// ClientConn is a client connection to one server. It dials on the first
// call and dials again on a later call once the connection has closed; calls
// in flight on a lost connection fail with Unavailable. A call that finds
// the connection closed before it has sent anything dials again and goes
// out on the new connection.
//
// When the server goes away (GOAWAY), later calls dial a new connection and
// the old one closes once its calls are done. A call the server refused
// meanwhile was not processed: a unary call goes out once more on the new
// connection, a streaming call fails with Unavailable. A streaming call
// keeps its connection until it ends or its context does.
//
// It is safe for concurrent use.
type ClientConn struct {
	dialer transport.Dialer
	// unaryInt and streamInt are the chains of interceptors, nil if empty.
	unaryInt  UnaryClientInterceptor
	streamInt StreamClientInterceptor

	mu sync.Mutex
	// conn takes new calls; nil before the first dial and after GOAWAY.
	conn *connection
	// conns holds every open connection, those going away included.
	conns   map[*connection]struct{}
	dialing chan struct{} // closed when the dial in progress ends
	closed  bool
}

// connection is a connection of a ClientConn. Its fields are guarded by
// ClientConn.mu.
type connection struct {
	transport.Conn
	// calls counts the calls on the connection, from before their stream
	// opens until they end.
	calls int
	// goingAway is set once the server sent GOAWAY or refused a call: the
	// connection takes no new calls and closes once calls drops to zero.
	goingAway bool
}

var _ ClientConnInterface = (*ClientConn)(nil)

// NewClient returns a ClientConn that dials with d; opts add interceptors.
// It does not dial yet.
func NewClient(d transport.Dialer, opts ...DialOption) *ClientConn {
	var o dialOptions
	for _, opt := range opts {
		opt(&o)
	}
	return &ClientConn{
		dialer:    d,
		unaryInt:  chainUnaryClient(o.unary),
		streamInt: chainStreamClient(o.stream),
		conns:     map[*connection]struct{}{},
	}
}

// Close closes the connections. Calls in flight and later calls fail with
// Canceled.
func (cc *ClientConn) Close() error {
	cc.mu.Lock()
	if cc.closed {
		cc.mu.Unlock()
		return errClientConnClosing
	}
	cc.closed = true
	cc.conn = nil
	conns := make([]*connection, 0, len(cc.conns))
	for c := range cc.conns {
		conns = append(conns, c)
	}
	cc.mu.Unlock()
	var err error
	for _, c := range conns {
		if cerr := c.CloseWithError(transport.NoError, "client connection closed"); err == nil {
			err = cerr
		}
	}
	return err
}

func (cc *ClientConn) isClosed() bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.closed
}

// getConn returns the connection that takes new calls, dialing if there is
// none, and counts a call on it in; the caller must release it. Concurrent
// callers share one dial; each of them waits only as long as its ctx allows.
func (cc *ClientConn) getConn(ctx context.Context) (*connection, error) {
	for {
		cc.mu.Lock()
		if cc.closed {
			cc.mu.Unlock()
			return nil, errClientConnClosing
		}
		if c := cc.conn; c != nil && c.Context().Err() == nil {
			c.calls++
			cc.mu.Unlock()
			return c, nil
		}
		if wait := cc.dialing; wait != nil {
			cc.mu.Unlock()
			select {
			case <-wait:
				continue
			case <-ctx.Done():
				return nil, status.FromContextError(ctx.Err()).Err()
			}
		}
		done := make(chan struct{})
		cc.dialing = done
		cc.mu.Unlock()

		conn, err := cc.dialer.Dial(ctx)

		cc.mu.Lock()
		cc.dialing = nil
		close(done)
		closed := cc.closed
		var c *connection
		if err == nil && !closed {
			c = &connection{Conn: conn, calls: 1}
			cc.conn = c
			cc.conns[c] = struct{}{}
		}
		cc.mu.Unlock()

		switch {
		case err != nil && ctx.Err() != nil:
			return nil, status.FromContextError(ctx.Err()).Err()
		case err != nil:
			return nil, status.Errorf(codes.Unavailable, "connection error: %v", err)
		case closed:
			_ = conn.CloseWithError(transport.NoError, "client connection closed")
			return nil, errClientConnClosing
		default:
			go cc.watch(c)
			return c, nil
		}
	}
}

// release counts a call on c out. The last call on a connection that is
// going away closes it.
func (cc *ClientConn) release(c *connection) {
	cc.mu.Lock()
	c.calls--
	last := c.goingAway && c.calls == 0
	cc.mu.Unlock()
	if last {
		_ = c.CloseWithError(transport.NoError, "")
	}
}

// goAway takes c out of use and closes it once its calls are done.
func (cc *ClientConn) goAway(c *connection) {
	cc.mu.Lock()
	idle := !c.goingAway && c.calls == 0
	c.goingAway = true
	if cc.conn == c {
		cc.conn = nil
	}
	cc.mu.Unlock()
	if idle {
		_ = c.CloseWithError(transport.NoError, "")
	}
}

// watch waits for GOAWAY on c, the only thing the server sends on a
// unidirectional stream, and forgets c once it closes.
func (cc *ClientConn) watch(c *connection) {
	defer func() {
		cc.mu.Lock()
		delete(cc.conns, c)
		if cc.conn == c {
			cc.conn = nil
		}
		cc.mu.Unlock()
	}()
	st, err := c.AcceptUniStream(c.Context())
	if err != nil {
		return
	}
	if err := call.ReadGoAway(frame.NewReader(st, frame.DefaultLimits())); err != nil {
		if c.Context().Err() == nil {
			_ = c.CloseWithError(transport.ProtocolError, err.Error())
		}
		return
	}
	cc.goAway(c)
	<-c.Context().Done()
}

var unaryStreamDesc = StreamDesc{}

// Invoke performs a unary call through the interceptors: it sends args and
// receives the response into reply.
func (cc *ClientConn) Invoke(ctx context.Context, method string, args, reply any, opts ...CallOption) error {
	if cc.unaryInt != nil {
		return cc.unaryInt(ctx, method, args, reply, cc, invoke, opts...)
	}
	return invoke(ctx, method, args, reply, cc, opts...)
}

// invoke performs a unary call after the interceptors. A call the server
// refused was not processed; it goes out once more, on a new connection, as
// grpc-go retries a stream refused with REFUSED_STREAM transparently
// (shouldRetry in stream.go). The interceptors see one call either way.
func invoke(ctx context.Context, method string, args, reply any, cc *ClientConn, opts ...CallOption) error {
	for retried := false; ; retried = true {
		cs, err := cc.newStream(ctx, &unaryStreamDesc, method, opts, args, true)
		if err != nil {
			return err
		}
		if err := cs.RecvMsg(reply); !cs.refused || retried {
			return err
		}
	}
}

// NewStream starts a streaming call described by desc, through the
// interceptors.
func (cc *ClientConn) NewStream(ctx context.Context, desc *StreamDesc, method string, opts ...CallOption) (ClientStream, error) {
	if cc.streamInt != nil {
		return cc.streamInt(ctx, desc, cc, method, newClientStream, opts...)
	}
	return newClientStream(ctx, desc, cc, method, opts...)
}

// newClientStream starts a streaming call after the interceptors.
func newClientStream(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, opts ...CallOption) (ClientStream, error) {
	cs, err := cc.newStream(ctx, desc, method, opts, nil, false)
	if err != nil {
		return nil, err
	}
	return cs, nil
}

// newStream opens a stream and sends HEADERS. For a unary call it sends the
// request and FIN in the same write.
func (cc *ClientConn) newStream(ctx context.Context, desc *StreamDesc, method string, opts []CallOption, req any, unary bool) (*clientStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	c, st, err := cc.openStream(ctx)
	if err != nil {
		return nil, err
	}
	abort := func() {
		st.CancelWrite(transport.Cancelled)
		st.CancelRead(transport.Cancelled)
		cc.release(c)
	}

	// The timeout is measured right before sending, after any wait for the
	// connection or for a free stream slot.
	var timeoutUs uint64
	if deadline, ok := ctx.Deadline(); ok {
		us, ok := call.TimeoutMicros(time.Until(deadline))
		if !ok {
			abort()
			return nil, status.Error(codes.DeadlineExceeded, context.DeadlineExceeded.Error())
		}
		timeoutUs = us
	}
	md, _ := metadata.FromOutgoingContext(ctx)
	buf, err := call.AppendRequestHeaders(nil, method, timeoutUs, md)
	if err == nil && unary {
		buf, err = call.AppendMessage(buf, req)
	}
	if err != nil {
		abort()
		return nil, status.Error(codes.Internal, err.Error())
	}

	cs := &clientStream{
		cc:          cc,
		conn:        c,
		ctx:         ctx,
		desc:        desc,
		st:          st,
		info:        newCallInfo(opts),
		headerReady: make(chan struct{}),
	}
	cs.recv = call.NewClientRecv(frame.NewReader(st, frame.DefaultLimits()), cs.noteHeader)
	cs.stopCancel = context.AfterFunc(ctx, func() {
		st.CancelWrite(transport.Cancelled)
		st.CancelRead(transport.Cancelled)
		cs.releaseConn()
	})
	// A failed write is not reported here: every cause (reset, lost
	// connection, cancellation) also ends the receive side, where RecvMsg
	// reports the precise status.
	if _, err := st.Write(buf); err == nil && unary {
		_ = cs.CloseSend()
	}
	return cs, nil
}

// openStream opens a stream, dialing if there is no connection, and
// returns the connection it is on, with the call counted in. The
// connection may have closed before the client noticed; a stream that fails
// to open on it has sent nothing, so the call dials again and tries once
// more (RPC-12). grpc-go retries such a call transparently as well:
// NewStreamError.AllowTransparentRetry (internal/transport/http2_client.go).
func (cc *ClientConn) openStream(ctx context.Context) (*connection, transport.Stream, error) {
	for retried := false; ; retried = true {
		c, err := cc.getConn(ctx)
		if err != nil {
			return nil, nil, err
		}
		st, err := c.OpenStream(ctx)
		if err == nil {
			return c, st, nil
		}
		cc.release(c)
		// The failed connection's context is canceled by now (see
		// transport.Conn), so getConn dials a new one.
		if _, closed := errors.AsType[*transport.ConnError](err); !closed || retried {
			return nil, nil, cc.openError(ctx, err)
		}
	}
}

func (cc *ClientConn) openError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return status.FromContextError(ctx.Err()).Err()
	}
	if cc.isClosed() {
		return errClientConnClosing
	}
	return status.Errorf(codes.Unavailable, "open stream: %v", err)
}

func newCallInfo(opts []CallOption) callInfo {
	var ci callInfo
	for _, o := range opts {
		o.apply(&ci)
	}
	return ci
}
