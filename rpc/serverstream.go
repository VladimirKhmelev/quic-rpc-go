package rpc

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/call"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

type serverStreamKey struct{}

// serverStream is the server side of one call over one transport stream.
type serverStream struct {
	ctx       context.Context
	st        transport.Stream
	recv      *call.ServerRecv
	desc      *StreamDesc // nil for unary methods
	stopAbort func() bool

	aborted    atomic.Bool
	gotMessage bool

	// mu guards the header and trailer state and serializes writes.
	mu         sync.Mutex
	header     metadata.MD
	headerSent bool // HEADERS or a MESSAGE went out; no HEADERS may follow
	trailer    metadata.MD
	ended      bool
	wbuf       []byte
}

func (ss *serverStream) Context() context.Context { return ss.ctx }

func (ss *serverStream) SetHeader(md metadata.MD) error {
	if md.Len() == 0 {
		return nil
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.headerSent || ss.ended {
		return status.Error(codes.Internal, "rpc: SetHeader called after the header was sent")
	}
	ss.header = metadata.Join(ss.header, md)
	return nil
}

func (ss *serverStream) SendHeader(md metadata.MD) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.headerSent || ss.ended {
		return status.Error(codes.Internal, "rpc: SendHeader called after the header was sent")
	}
	header := metadata.Join(ss.header, md)
	buf, err := call.AppendResponseHeaders(ss.wbuf[:0], header)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	ss.header, ss.headerSent, ss.wbuf = header, true, buf
	if _, err := ss.st.Write(buf); err != nil {
		return ss.writeFailed(err)
	}
	return nil
}

func (ss *serverStream) SetTrailer(md metadata.MD) {
	if md.Len() == 0 {
		return
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	ss.trailer = metadata.Join(ss.trailer, md)
}

func (ss *serverStream) SendMsg(m any) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.ended || ss.aborted.Load() {
		return ss.writeFailed(errors.New("the call is over"))
	}
	buf := ss.wbuf[:0]
	var err error
	if !ss.headerSent && ss.header.Len() > 0 {
		if buf, err = call.AppendResponseHeaders(buf, ss.header); err != nil {
			return status.Error(codes.Internal, err.Error())
		}
	}
	if buf, err = call.AppendMessage(buf, m); err != nil {
		return status.Errorf(codes.Internal, "marshal response: %v", err)
	}
	ss.headerSent, ss.wbuf = true, buf
	if _, err := ss.st.Write(buf); err != nil {
		return ss.writeFailed(err)
	}
	return nil
}

func (ss *serverStream) RecvMsg(m any) error {
	payload, err := ss.recv.Next()
	if errors.Is(err, io.EOF) {
		if !ss.desc.ClientStreams && !ss.gotMessage {
			return status.Error(codes.Internal, "cardinality violation: received no request message from non-client-streaming RPC")
		}
		return io.EOF
	}
	if err != nil {
		return ss.recvFailed(err).Err()
	}
	if !ss.desc.ClientStreams {
		// The only request message must be followed by the client's FIN.
		if _, err := ss.recv.Next(); !errors.Is(err, io.EOF) {
			if err == nil {
				return status.Error(codes.Internal, "cardinality violation: received multiple request messages for non-client-streaming RPC")
			}
			return ss.recvFailed(err).Err()
		}
	}
	ss.gotMessage = true
	if err := call.Unmarshal(payload, m); err != nil {
		return status.Errorf(codes.Internal, "unmarshal request: %v", err)
	}
	return nil
}

// recvFailed handles an error from reading the client's frames and returns
// the status for it. An oversized message only fails the call; anything
// else means the stream is broken and is reset.
func (ss *serverStream) recvFailed(err error) *status.Status {
	if tl, ok := errors.AsType[*frame.TooLargeError](err); ok {
		return status.Newf(codes.ResourceExhausted, "received message larger than max (%d vs. %d)", tl.Size, tl.Limit)
	}
	code := call.ResetCode(err)
	ss.abort(code)
	if code == transport.ProtocolError {
		return status.New(codes.Internal, err.Error())
	}
	if ctxErr := ss.ctx.Err(); ctxErr != nil {
		return status.FromContextError(ctxErr)
	}
	return status.New(codes.Canceled, err.Error())
}

// writeFailed returns the status for a failed write. A reset means the
// client canceled: the STOP_SENDING may arrive before the handler's context
// is canceled, which happens asynchronously.
func (ss *serverStream) writeFailed(err error) error {
	if ctxErr := ss.ctx.Err(); ctxErr != nil {
		return status.FromContextError(ctxErr).Err()
	}
	if _, ok := errors.AsType[*transport.StreamError](err); ok {
		return status.Error(codes.Canceled, err.Error())
	}
	return status.Error(codes.Unavailable, err.Error())
}

// abort resets both directions. It takes no lock: it must unblock a SendMsg
// that holds mu while stuck in flow control.
func (ss *serverStream) abort(code transport.ErrorCode) {
	ss.aborted.Store(true)
	ss.st.CancelWrite(code)
	ss.st.CancelRead(code)
}

// finish ends the call with st (nil means OK).
func (ss *serverStream) finish(st *status.Status) { ss.end(nil, false, st) }

// finishUnary ends a successful unary call with its response.
func (ss *serverStream) finishUnary(resp any) { ss.end(resp, true, nil) }

// end writes the pending header, the response if sendResp, TRAILERS with st
// and FIN, in one write. It does nothing if the stream was reset.
func (ss *serverStream) end(resp any, sendResp bool, st *status.Status) {
	if !ss.stopAbort() || ss.aborted.Load() {
		return
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.ended {
		return
	}
	ss.ended = true

	buf := ss.wbuf[:0]
	if !ss.headerSent && ss.header.Len() > 0 {
		b, err := call.AppendResponseHeaders(buf, ss.header)
		if err != nil {
			st = status.New(codes.Internal, err.Error())
		} else {
			buf = b
		}
	}
	if sendResp && st == nil {
		b, err := call.AppendMessage(buf, resp)
		if err != nil {
			st = status.Newf(codes.Internal, "marshal response: %v", err)
		} else {
			buf = b
		}
	}
	out, err := call.AppendTrailers(buf, st, ss.trailer)
	if err != nil {
		out, _ = call.AppendTrailers(buf, status.New(codes.Internal, err.Error()), nil)
	}
	_, _ = ss.st.Write(out)
	_ = ss.st.Close()
	if !ss.recv.Done() {
		// The client may still be sending; it does not need to.
		ss.st.CancelRead(transport.NoError)
	}
}
