// Package transport defines the byte-stream transport the RPC layer runs on:
// connections that carry many bidirectional streams with QUIC stream
// semantics — half-close, reset, stop sending and flow control.
//
// Implementations: transport/quic (package rpcquic) on top of quic-go, and
// transport/mem, an in-process transport for tests.
package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
)

// ErrorCode is an application error code carried by RESET_STREAM,
// STOP_SENDING and CONNECTION_CLOSE. See docs/protocol.md.
type ErrorCode uint64

// Error codes defined by the protocol.
const (
	NoError       ErrorCode = 0x00
	Cancelled     ErrorCode = 0x01
	ProtocolError ErrorCode = 0x02
	InternalError ErrorCode = 0x03
)

func (c ErrorCode) String() string {
	switch c {
	case NoError:
		return "NO_ERROR"
	case Cancelled:
		return "CANCELLED"
	case ProtocolError:
		return "PROTOCOL_ERROR"
	case InternalError:
		return "INTERNAL_ERROR"
	default:
		return fmt.Sprintf("ErrorCode(%#x)", uint64(c))
	}
}

// Stream is one bidirectional stream. Read and Write may be called
// concurrently with each other and with the other methods, but Read must not
// be called concurrently with Read, nor Write with Write.
type Stream interface {
	// Read reads data sent by the peer. It returns io.EOF after the peer
	// closed its send direction and all data has been read, a *StreamError
	// after a reset, and a *ConnError after the connection closed.
	io.Reader
	// Write blocks while the peer has no room for more data (flow control).
	// It returns a *StreamError after a reset and a *ConnError after the
	// connection closed. Write after Close returns a non-nil error.
	io.Writer
	// Close closes the send direction (FIN). Reading is not affected.
	Close() error
	// CancelWrite aborts the send direction; unsent data is discarded and
	// the peer's Read fails with a remote *StreamError carrying code.
	CancelWrite(code ErrorCode)
	// CancelRead aborts the receive direction and asks the peer to stop
	// sending; the peer's Write fails with a remote *StreamError carrying code.
	CancelRead(code ErrorCode)
	// Context is canceled once the send direction is done. Its cause is
	// ErrStreamClosed after Close, a *StreamError after CancelWrite or the
	// peer's CancelRead, and a *ConnError after the connection closed.
	Context() context.Context
}

// Conn is a connection that multiplexes streams. Streams are opened by the
// client side only.
type Conn interface {
	// OpenStream opens a stream, waiting while the peer's limit on
	// concurrent streams is reached.
	OpenStream(ctx context.Context) (Stream, error)
	// AcceptStream waits for the next stream opened by the peer.
	AcceptStream(ctx context.Context) (Stream, error)
	// CloseWithError closes the connection; the peer sees code and reason
	// in a remote *ConnError.
	CloseWithError(code ErrorCode, reason string) error
	// Context is canceled when the connection closes; its cause is a
	// *ConnError. It is canceled before CloseWithError returns and before
	// any method of the connection or of its streams returns a *ConnError,
	// so a caller that checks Context never reuses a connection it has
	// seen closed.
	Context() context.Context
}

// Listener accepts incoming connections.
type Listener interface {
	// Accept waits for the next connection. After Close it returns
	// ErrListenerClosed.
	Accept(ctx context.Context) (Conn, error)
	// Close stops accepting connections. Accepted connections stay open.
	Close() error
	Addr() net.Addr
}

// Dialer establishes connections to one server.
type Dialer interface {
	Dial(ctx context.Context) (Conn, error)
}

// ErrListenerClosed is returned by Listener.Accept after Close.
var ErrListenerClosed = errors.New("transport: listener closed")

// ErrStreamClosed is the cause of a stream's context after Close.
var ErrStreamClosed = errors.New("transport: stream closed")

// StreamError reports that a stream direction was reset, by this side
// (Remote false) or by the peer (Remote true).
type StreamError struct {
	Code   ErrorCode
	Remote bool
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("transport: stream reset by %s side: %v", side(e.Remote), e.Code)
}

// ConnError reports that the connection is closed.
type ConnError struct {
	// Code, Remote and Reason describe a close by CloseWithError on this
	// side (Remote false) or on the peer (Remote true). They are valid only
	// if Err is nil.
	Code   ErrorCode
	Remote bool
	Reason string
	// Err is the cause of any other close, such as an idle timeout.
	Err error
}

func (e *ConnError) Error() string {
	if e.Err != nil {
		return "transport: connection closed: " + e.Err.Error()
	}
	return fmt.Sprintf("transport: connection closed by %s side: %v %q", side(e.Remote), e.Code, e.Reason)
}

func (e *ConnError) Unwrap() error { return e.Err }

func side(remote bool) string {
	if remote {
		return "remote"
	}
	return "local"
}
