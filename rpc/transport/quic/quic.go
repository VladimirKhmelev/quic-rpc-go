// Package rpcquic is the QUIC transport of quic-rpc-go, built on quic-go.
//
// Every call is one bidirectional QUIC stream opened by the client. The
// package takes copies of the caller's TLS and QUIC configurations and
// adjusts them for the protocol: the ALPN is set to "qrpc/1", unidirectional
// streams are disabled and the client accepts no streams from the server.
// All other settings are kept.
package rpcquic

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/quic-go/quic-go"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// ALPN is the TLS application protocol of quic-rpc-go, version 1.
const ALPN = "qrpc/1"

// Listener accepts QUIC connections.
type Listener struct {
	l *quic.Listener
}

// Listen listens for connections on the UDP address addr. tlsConf must
// contain the server certificate; conf may be nil.
func Listen(addr string, tlsConf *tls.Config, conf *quic.Config) (*Listener, error) {
	tc, err := configureTLS(tlsConf)
	if err != nil {
		return nil, err
	}
	l, err := quic.ListenAddr(addr, tc, configureQUIC(conf, false))
	if err != nil {
		return nil, fmt.Errorf("rpcquic: listen on %s: %w", addr, err)
	}
	return &Listener{l: l}, nil
}

// Accept waits for the next connection.
func (l *Listener) Accept(ctx context.Context) (transport.Conn, error) {
	qc, err := l.l.Accept(ctx)
	if err != nil {
		if errors.Is(err, quic.ErrServerClosed) {
			return nil, transport.ErrListenerClosed
		}
		return nil, err
	}
	return newConn(qc), nil
}

// Close stops accepting connections. Accepted connections stay open.
func (l *Listener) Close() error { return l.l.Close() }

// Addr returns the local UDP address.
func (l *Listener) Addr() net.Addr { return l.l.Addr() }

// Dialer dials QUIC connections to one server.
type Dialer struct {
	// Addr is the server's UDP address.
	Addr string
	// TLSConfig must allow verifying the server certificate.
	TLSConfig *tls.Config
	// QUICConfig may be nil.
	QUICConfig *quic.Config
}

// Dial establishes a connection and completes the handshake.
func (d *Dialer) Dial(ctx context.Context) (transport.Conn, error) {
	tc, err := configureTLS(d.TLSConfig)
	if err != nil {
		return nil, err
	}
	qc, err := quic.DialAddr(ctx, d.Addr, tc, configureQUIC(d.QUICConfig, true))
	if err != nil {
		return nil, fmt.Errorf("rpcquic: dial %s: %w", d.Addr, err)
	}
	return newConn(qc), nil
}

func configureTLS(c *tls.Config) (*tls.Config, error) {
	if c == nil {
		return nil, errors.New("rpcquic: a TLS config is required")
	}
	c = c.Clone()
	switch {
	case len(c.NextProtos) == 0:
		c.NextProtos = []string{ALPN}
	case len(c.NextProtos) == 1 && c.NextProtos[0] == ALPN:
	default:
		return nil, fmt.Errorf("rpcquic: tls.Config.NextProtos must be empty or [%q], got %q", ALPN, c.NextProtos)
	}
	return c, nil
}

func configureQUIC(c *quic.Config, client bool) *quic.Config {
	if c == nil {
		c = &quic.Config{}
	} else {
		c = c.Clone()
	}
	c.MaxIncomingUniStreams = -1
	if client {
		c.MaxIncomingStreams = -1
	}
	return c
}

// conn keeps its own context so that it can cancel it as soon as anything
// reports the connection closed. quic-go v0.63.0 fails stream operations
// once it shuts the streams down (Conn.handleCloseError, connection.go:2258)
// but cancels its own context only when Conn.run returns
// (connection.go:564), and newConn forwards that cancellation from yet
// another goroutine.
type conn struct {
	qc     *quic.Conn
	ctx    context.Context
	cancel context.CancelCauseFunc
}

func newConn(qc *quic.Conn) *conn {
	ctx, cancel := context.WithCancelCause(context.Background())
	qctx := qc.Context()
	context.AfterFunc(qctx, func() { cancel(connError(context.Cause(qctx))) })
	return &conn{qc: qc, ctx: ctx, cancel: cancel}
}

func (c *conn) OpenStream(ctx context.Context) (transport.Stream, error) {
	qs, err := c.qc.OpenStreamSync(ctx)
	if err != nil {
		return nil, c.convert(err)
	}
	return &stream{c: c, qs: qs}, nil
}

func (c *conn) AcceptStream(ctx context.Context) (transport.Stream, error) {
	qs, err := c.qc.AcceptStream(ctx)
	if err != nil {
		return nil, c.convert(err)
	}
	return &stream{c: c, qs: qs}, nil
}

// CloseWithError returns once quic-go has canceled its context
// (Conn.CloseWithError waits for it), so the cause is known here.
func (c *conn) CloseWithError(code transport.ErrorCode, reason string) error {
	err := c.convert(c.qc.CloseWithError(quic.ApplicationErrorCode(code), reason))
	if qctx := c.qc.Context(); qctx.Err() != nil {
		c.cancel(connError(context.Cause(qctx)))
	}
	return err
}

func (c *conn) Context() context.Context { return c.ctx }

// convert is convertError that also cancels the connection context when err
// reports the connection closed.
func (c *conn) convert(err error) error {
	err = convertError(err)
	if ce, ok := errors.AsType[*transport.ConnError](err); ok {
		c.cancel(ce)
	}
	return err
}

type stream struct {
	c  *conn
	qs *quic.Stream

	ctxOnce sync.Once
	ctx     context.Context
}

func (s *stream) Read(b []byte) (int, error) {
	n, err := s.qs.Read(b)
	return n, s.c.convert(err)
}

func (s *stream) Write(b []byte) (int, error) {
	n, err := s.qs.Write(b)
	return n, s.c.convert(err)
}

func (s *stream) Close() error { return s.c.convert(s.qs.Close()) }

func (s *stream) CancelWrite(code transport.ErrorCode) {
	s.qs.CancelWrite(quic.StreamErrorCode(code))
}

func (s *stream) CancelRead(code transport.ErrorCode) {
	s.qs.CancelRead(quic.StreamErrorCode(code))
}

// Context translates the cause of quic-go's stream context. It is built on
// first use: the client side of a call never needs it.
func (s *stream) Context() context.Context {
	s.ctxOnce.Do(func() {
		qctx := s.qs.Context()
		ctx, cancel := context.WithCancelCause(context.Background())
		if qctx.Err() != nil {
			cancel(streamCause(context.Cause(qctx)))
		} else {
			context.AfterFunc(qctx, func() { cancel(streamCause(context.Cause(qctx))) })
		}
		s.ctx = ctx
	})
	return s.ctx
}

// streamCause converts the cause of quic-go's stream context. quic-go
// v0.63.0 cancels it without a cause on Close, so context.Canceled means
// the stream was closed.
func streamCause(err error) error {
	if errors.Is(err, context.Canceled) {
		return transport.ErrStreamClosed
	}
	if se, ok := errors.AsType[*quic.StreamError](err); ok {
		return streamError(se)
	}
	return connError(err)
}

// convertError maps quic-go stream and connection errors to transport
// errors; everything else, including io.EOF and context errors, passes through.
func convertError(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	if se, ok := errors.AsType[*quic.StreamError](err); ok {
		return streamError(se)
	}
	if isConnClosed(err) {
		return connError(err)
	}
	return err
}

func streamError(se *quic.StreamError) *transport.StreamError {
	return &transport.StreamError{Code: transport.ErrorCode(se.ErrorCode), Remote: se.Remote}
}

func connError(err error) *transport.ConnError {
	if ae, ok := errors.AsType[*quic.ApplicationError](err); ok {
		return &transport.ConnError{Code: transport.ErrorCode(ae.ErrorCode), Remote: ae.Remote, Reason: ae.ErrorMessage}
	}
	return &transport.ConnError{Err: err}
}

// isConnClosed reports whether err is one of the errors quic-go uses for a
// closed connection.
func isConnClosed(err error) bool {
	return is[*quic.ApplicationError](err) || is[*quic.TransportError](err) ||
		is[*quic.IdleTimeoutError](err) || is[*quic.HandshakeTimeoutError](err) ||
		is[*quic.StatelessResetError](err) || is[*quic.VersionNegotiationError](err)
}

func is[T error](err error) bool {
	_, ok := errors.AsType[T](err)
	return ok
}
