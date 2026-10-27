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
// out on the new connection. It is safe for concurrent use.
type ClientConn struct {
	dialer transport.Dialer

	mu      sync.Mutex
	conn    transport.Conn
	dialing chan struct{} // closed when the dial in progress ends
	closed  bool
}

var _ ClientConnInterface = (*ClientConn)(nil)

// NewClient returns a ClientConn that dials with d. It does not dial yet.
func NewClient(d transport.Dialer) *ClientConn {
	return &ClientConn{dialer: d}
}

// Close closes the connection. Calls in flight and later calls fail with
// Canceled.
func (cc *ClientConn) Close() error {
	cc.mu.Lock()
	if cc.closed {
		cc.mu.Unlock()
		return errClientConnClosing
	}
	cc.closed = true
	conn := cc.conn
	cc.conn = nil
	cc.mu.Unlock()
	if conn != nil {
		return conn.CloseWithError(transport.NoError, "client connection closed")
	}
	return nil
}

func (cc *ClientConn) isClosed() bool {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.closed
}

// getConn returns the open connection, dialing if there is none. Concurrent
// callers share one dial; each of them waits only as long as its ctx allows.
func (cc *ClientConn) getConn(ctx context.Context) (transport.Conn, error) {
	for {
		cc.mu.Lock()
		if cc.closed {
			cc.mu.Unlock()
			return nil, errClientConnClosing
		}
		if cc.conn != nil && cc.conn.Context().Err() == nil {
			conn := cc.conn
			cc.mu.Unlock()
			return conn, nil
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
		if err == nil && !closed {
			cc.conn = conn
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
			return conn, nil
		}
	}
}

var unaryStreamDesc = StreamDesc{}

// Invoke performs a unary call: it sends args and receives the response
// into reply.
func (cc *ClientConn) Invoke(ctx context.Context, method string, args, reply any, opts ...CallOption) error {
	cs, err := cc.newStream(ctx, &unaryStreamDesc, method, opts, args, true)
	if err != nil {
		return err
	}
	return cs.RecvMsg(reply)
}

// NewStream starts a streaming call described by desc.
func (cc *ClientConn) NewStream(ctx context.Context, desc *StreamDesc, method string, opts ...CallOption) (ClientStream, error) {
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
	st, err := cc.openStream(ctx)
	if err != nil {
		return nil, err
	}

	// The timeout is measured right before sending, after any wait for the
	// connection or for a free stream slot.
	var timeoutUs uint64
	if deadline, ok := ctx.Deadline(); ok {
		us, ok := call.TimeoutMicros(time.Until(deadline))
		if !ok {
			st.CancelWrite(transport.Cancelled)
			st.CancelRead(transport.Cancelled)
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
		st.CancelWrite(transport.Cancelled)
		st.CancelRead(transport.Cancelled)
		return nil, status.Error(codes.Internal, err.Error())
	}

	cs := &clientStream{
		cc:          cc,
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
	})
	// A failed write is not reported here: every cause (reset, lost
	// connection, cancellation) also ends the receive side, where RecvMsg
	// reports the precise status.
	if _, err := st.Write(buf); err == nil && unary {
		_ = cs.CloseSend()
	}
	return cs, nil
}

// openStream opens a stream, dialing if there is no connection. The
// connection may have closed before the client noticed; a stream that fails
// to open on it has sent nothing, so the call dials again and tries once
// more (RPC-12). grpc-go retries such a call transparently as well:
// NewStreamError.AllowTransparentRetry (internal/transport/http2_client.go).
func (cc *ClientConn) openStream(ctx context.Context) (transport.Stream, error) {
	for retried := false; ; retried = true {
		conn, err := cc.getConn(ctx)
		if err != nil {
			return nil, err
		}
		st, err := conn.OpenStream(ctx)
		if err == nil {
			return st, nil
		}
		// The failed connection's context is canceled by now (see
		// transport.Conn), so getConn dials a new one.
		if _, closed := errors.AsType[*transport.ConnError](err); !closed || retried {
			return nil, cc.openError(ctx, err)
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
