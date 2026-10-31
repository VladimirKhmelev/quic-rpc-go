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
	Refused       ErrorCode = 0x04
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
	case Refused:
		return "REFUSED"
	default:
		return fmt.Sprintf("ErrorCode(%#x)", uint64(c))
	}
}

// ReceiveStream is the receive direction of a stream. Read must not be
// called concurrently with Read.
type ReceiveStream interface {
	// Read reads data sent by the peer. It returns io.EOF after the peer
	// closed its send direction and all data has been read, a *StreamError
	// after a reset, and a *ConnError after the connection closed.
	io.Reader
	// CancelRead aborts the receive direction and asks the peer to stop
	// sending; the peer's Write fails with a remote *StreamError carrying code.
	CancelRead(code ErrorCode)
}

// SendStream is the send direction of a stream. Write must not be called
// concurrently with Write.
type SendStream interface {
	// Write blocks while the peer has no room for more data (flow control).
	// It returns a *StreamError after a reset and a *ConnError after the
	// connection closed. Write after Close returns a non-nil error.
	io.Writer
	// Close closes the send direction (FIN).
	Close() error
	// CancelWrite aborts the send direction; unsent data is discarded and
	// the peer's Read fails with a remote *StreamError carrying code.
	CancelWrite(code ErrorCode)
	// Context is canceled once the send direction is done. Its cause is
	// ErrStreamClosed after Close, a *StreamError after CancelWrite or the
	// peer's CancelRead, and a *ConnError after the connection closed.
	Context() context.Context
}

// Stream is one bidirectional stream. Its two directions are independent:
// Read and Write may be called concurrently with each other and with the
// other methods, and closing or canceling one direction leaves the other
// as it is.
type Stream interface {
	ReceiveStream
	SendStream
}

// Conn is a connection that multiplexes streams. Bidirectional streams are
// opened by the client side only, unidirectional ones by the server side
// only, and the client takes one unidirectional stream at a time.
type Conn interface {
	// OpenStream opens a stream, waiting while the peer's limit on
	// concurrent streams is reached.
	OpenStream(ctx context.Context) (Stream, error)
	// AcceptStream waits for the next stream opened by the peer.
	AcceptStream(ctx context.Context) (Stream, error)
	// OpenUniStream opens a unidirectional stream, waiting while the peer
	// has one open.
	OpenUniStream(ctx context.Context) (SendStream, error)
	// AcceptUniStream waits for the next unidirectional stream opened by
	// the peer.
	AcceptUniStream(ctx context.Context) (ReceiveStream, error)
	// CloseWithError closes the connection; the peer sees code and reason
	// in a remote *ConnError. It discards what the peer has not read yet:
	// the peer's reads fail even on data that has arrived.
	CloseWithError(code ErrorCode, reason string) error
	// Context is canceled when the connection closes; its cause is a
	// *ConnError. It is canceled before CloseWithError returns and before
	// any method of the connection or of its streams returns a *ConnError,
	// so a caller that checks Context never reuses a connection it has
	// seen closed.
	Context() context.Context
}

// EarlyConn is an optional capability for replayable early data. OpenStream
// on such a connection still waits for the handshake. Implementations without
// this capability have no early data phase.
type EarlyConn interface {
	Conn
	// OpenStreamEarly may send data before the peer is authenticated. Only
	// idempotent calls, including their metadata, may use these streams.
	// Rejection invalidates existing early streams with ErrEarlyDataRejected;
	// later opens recover on the same connection after the handshake.
	OpenStreamEarly(ctx context.Context) (Stream, error)
	// WaitHandshake waits for successful authentication and recovery from
	// early data rejection. Canceling ctx does not close the connection.
	WaitHandshake(ctx context.Context) error
}

// ErrEarlyDataRejected means the peer discarded all early data without
// processing it. The connection remains usable after WaitHandshake, but the
// rejected streams cannot be reused. It is not a connection failure.
var ErrEarlyDataRejected = errors.New("early data rejected")

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
