package rpc

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/call"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// clientStream is the client side of one call over one transport stream.
type clientStream struct {
	cc         *ClientConn
	ctx        context.Context
	desc       *StreamDesc
	st         transport.Stream
	recv       *call.ClientRecv
	info       callInfo
	stopCancel func() bool

	sendMu sync.Mutex // serializes SendMsg and CloseSend
	wbuf   []byte
	// sendClosed is set by whoever finishes the send direction: CloseSend
	// with FIN or finish with a reset.
	sendClosed atomic.Bool

	recvMu     sync.Mutex
	gotMessage bool
	finalErr   error // what RecvMsg returns once the call ended

	ended       atomic.Bool
	headerOnce  sync.Once
	headerReady chan struct{} // closed once the header is known or the call ended
	header      metadata.MD
	headerErr   error
	trailer     atomic.Pointer[metadata.MD]
}

func (cs *clientStream) Context() context.Context { return cs.ctx }

func (cs *clientStream) SendMsg(m any) error {
	cs.sendMu.Lock()
	defer cs.sendMu.Unlock()
	if cs.ended.Load() || cs.sendClosed.Load() {
		return io.EOF
	}
	buf, err := call.AppendMessage(cs.wbuf[:0], m)
	if err != nil {
		return status.Errorf(codes.Internal, "marshal message: %v", err)
	}
	cs.wbuf = buf
	if _, err := cs.st.Write(buf); err != nil {
		// The call is over or failing; RecvMsg reports why.
		return io.EOF
	}
	return nil
}

func (cs *clientStream) CloseSend() error {
	cs.sendMu.Lock()
	defer cs.sendMu.Unlock()
	if !cs.sendClosed.Swap(true) {
		_ = cs.st.Close()
	}
	return nil
}

func (cs *clientStream) RecvMsg(m any) error {
	cs.recvMu.Lock()
	defer cs.recvMu.Unlock()
	if cs.finalErr != nil {
		return cs.finalErr
	}
	// No message is handed out once the call was canceled, even one that
	// already arrived: the reset runs in an AfterFunc goroutine and may not
	// have discarded it yet, and Header may have read it ahead. grpc-go has
	// no such check; TST-3 requires it.
	if err := cs.ctx.Err(); err != nil {
		return cs.fail(status.FromContextError(err), transport.Cancelled)
	}
	payload, err := cs.recv.Next()
	if err != nil {
		return cs.end(err)
	}
	cs.gotMessage = true
	if err := call.Unmarshal(payload, m); err != nil {
		return cs.fail(status.Newf(codes.Internal, "unmarshal response: %v", err), transport.Cancelled)
	}
	if cs.desc.ServerStreams {
		return nil
	}
	// One response message, then the status.
	if _, err := cs.recv.Next(); err == nil {
		return cs.fail(status.New(codes.Internal,
			"cardinality violation: expected <EOF> for non server-streaming RPCs, but received another message"), transport.Cancelled)
	} else if err := cs.end(err); !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// end finishes the call after the receive side returned err: io.EOF means
// TRAILERS were read and carry the status.
func (cs *clientStream) end(err error) error {
	if !errors.Is(err, io.EOF) {
		return cs.fail(cs.statusFor(err), call.ResetCode(err))
	}
	st := cs.recv.Status()
	if st.Code() == codes.OK && !cs.desc.ServerStreams && !cs.gotMessage {
		st = status.New(codes.Internal, "cardinality violation: received no response message from non-server-streaming RPC")
	}
	cs.finish(st)
	if st.Code() == codes.OK {
		cs.finalErr = io.EOF
	} else {
		cs.finalErr = st.Err()
	}
	return cs.finalErr
}

// fail ends the call with st and resets the stream.
func (cs *clientStream) fail(st *status.Status, code transport.ErrorCode) error {
	cs.st.CancelWrite(code)
	cs.st.CancelRead(code)
	cs.finish(st)
	cs.finalErr = st.Err()
	return cs.finalErr
}

func (cs *clientStream) statusFor(err error) *status.Status {
	if ctxErr := cs.ctx.Err(); ctxErr != nil {
		return status.FromContextError(ctxErr)
	}
	if _, ok := errors.AsType[*transport.ConnError](err); ok && cs.cc.isClosed() {
		return status.Convert(errClientConnClosing)
	}
	// The server resets a call whose deadline expired, possibly before our
	// own context noticed the same deadline; grpc-go does the same
	// (internal/transport/http2_client.go).
	if se, ok := errors.AsType[*transport.StreamError](err); ok && se.Remote && se.Code == transport.Cancelled {
		if deadline, ok := cs.ctx.Deadline(); ok && !deadline.After(time.Now()) {
			return status.New(codes.DeadlineExceeded, context.DeadlineExceeded.Error())
		}
	}
	return call.ClientStatus(err)
}

// finish records the outcome. It runs once, with recvMu held.
func (cs *clientStream) finish(st *status.Status) {
	if cs.ended.Swap(true) {
		return
	}
	cs.stopCancel()
	// Done with the stream: a send direction left open would keep the
	// stream's slot in the peer's stream limit. CancelWrite needs no
	// sendMu: it also unblocks a SendMsg stuck in flow control.
	if !cs.sendClosed.Swap(true) {
		cs.st.CancelWrite(transport.NoError)
	}

	trailer := metadata.MD(cs.recv.Trailer())
	cs.trailer.Store(&trailer)
	cs.headerOnce.Do(func() {
		if st.Code() != codes.OK && !cs.recv.HeaderKnown() {
			cs.headerErr = st.Err()
		}
		close(cs.headerReady)
	})
	if cs.info.header != nil {
		*cs.info.header = cs.header
	}
	if cs.info.trailer != nil {
		*cs.info.trailer = trailer
	}
}

// noteHeader runs when the receive side learns what header the server sent.
func (cs *clientStream) noteHeader() {
	cs.headerOnce.Do(func() {
		if h := cs.recv.Header(); h != nil {
			cs.header = metadata.MD(h)
		}
		close(cs.headerReady)
	})
}

func (cs *clientStream) Header() (metadata.MD, error) {
	for {
		select {
		case <-cs.headerReady:
			return cs.header, cs.headerErr
		default:
		}
		// Read ahead unless another goroutine is receiving; it will close
		// headerReady as soon as the header is known.
		if cs.recvMu.TryLock() {
			if cs.finalErr == nil {
				if err := cs.recv.ReadHeader(); err != nil {
					_ = cs.end(err)
				}
			}
			cs.recvMu.Unlock()
			continue
		}
		<-cs.headerReady
	}
}

func (cs *clientStream) Trailer() metadata.MD {
	if t := cs.trailer.Load(); t != nil {
		return *t
	}
	return nil
}
