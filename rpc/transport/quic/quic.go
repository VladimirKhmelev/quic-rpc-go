// Package rpcquic is the QUIC transport of quic-rpc-go, built on quic-go.
//
// Every call is one bidirectional QUIC stream opened by the client. The
// package takes copies of the caller's TLS and QUIC configurations and
// adjusts them for the protocol: the ALPN is set to "qrpc/2", the server
// accepts no unidirectional streams, and the client accepts no
// bidirectional streams and one unidirectional stream at a time, which the
// server opens to send GOAWAY. The client also sends keepalives unless told
// otherwise (see Dialer). All other settings are kept.
//
// A server that restarts should listen with NewListener on a transport with
// a stateless reset key that stays the same across restarts. When a server
// dies, its clients are not told, as they would be of a TCP connection
// closed by the kernel: their packets reach the new server, which knows
// nothing of the connection. With the key it answers them with a stateless
// reset (RFC 9000, section 10.3) and the client closes the connection at
// once; without it the packets are dropped and the client waits for the
// idle timeout, 30 s by default. Either way the streams that reach the new
// server fail: the client learns of the restart only when it sends.
package rpcquic

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// ALPN is the TLS application protocol of quic-rpc-go, version 2.
const ALPN = "qrpc/2"

// Listener accepts QUIC connections.
type Listener struct {
	l interface {
		Accept(context.Context) (*quic.Conn, error)
		Close() error
		Addr() net.Addr
	}
	waitHandshake bool
}

// Listen listens for connections on the UDP address addr. tlsConf must
// contain the server certificate; conf may be nil. The server sends no
// stateless resets; see NewListener. Allow0RTT enables early connections.
// With client certificates, custom TLS verification or GetConfigForClient,
// streams are accepted only after the handshake authenticates the client.
func Listen(addr string, tlsConf *tls.Config, conf *quic.Config) (*Listener, error) {
	tc, err := configureTLS(tlsConf)
	if err != nil {
		return nil, err
	}
	qc := configureQUIC(conf, false)
	l := &Listener{waitHandshake: needsClientAuthentication(tc)}
	if qc.Allow0RTT {
		l.l, err = quic.ListenAddrEarly(addr, tc, qc)
	} else {
		l.l, err = quic.ListenAddr(addr, tc, qc)
	}
	if err != nil {
		return nil, fmt.Errorf("rpcquic: listen on %s: %w", addr, err)
	}
	return l, nil
}

// NewListener accepts connections on t, a transport the caller set up, as
// Listen does on an address. The caller closes t after the listener;
// closing t ends its connections at once, without telling the clients.
// Set t.StatelessResetKey to a key that is the same on every start of the
// server, so that clients of a restarted server close their connections
// at once (see the package documentation).
func NewListener(t *quic.Transport, tlsConf *tls.Config, conf *quic.Config) (*Listener, error) {
	tc, err := configureTLS(tlsConf)
	if err != nil {
		return nil, err
	}
	qc := configureQUIC(conf, false)
	l := &Listener{waitHandshake: needsClientAuthentication(tc)}
	if qc.Allow0RTT {
		l.l, err = t.ListenEarly(tc, qc)
	} else {
		l.l, err = t.Listen(tc, qc)
	}
	if err != nil {
		return nil, fmt.Errorf("rpcquic: listen: %w", err)
	}
	return l, nil
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
	c := newConn(qc)
	c.waitHandshake = l.waitHandshake
	return c, nil
}

// Close stops accepting connections. Accepted connections stay open.
func (l *Listener) Close() error { return l.l.Close() }

// Addr returns the local UDP address.
func (l *Listener) Addr() net.Addr { return l.l.Addr() }

// Dialer dials QUIC connections to one server. It is safe for concurrent Dial
// calls. Do not copy it or change its configuration after its first use.
type Dialer struct {
	// Addr is the server's UDP address.
	Addr string
	// TLSConfig must allow verifying the server certificate.
	TLSConfig *tls.Config
	// QUICConfig may be nil. A zero KeepAlivePeriod means half of
	// MaxIdleTimeout, unlike in quic-go, where it turns keepalives off; a
	// negative one turns them off. Without keepalives quic-go closes a
	// connection that has been idle for the idle timeout, 30 s by default,
	// and the next call pays for a handshake; with them an idle connection
	// stays open, as TCP connections of grpc-go do, and the connection to a
	// dead server closes after the idle timeout even without calls.
	QUICConfig *quic.Config
	// Enable0RTT allows idempotent calls to send early data when reconnecting.
	// The server must allow it too. TLSConfig.ClientSessionCache is used if
	// set; otherwise the dialer keeps a one-entry cache for its lifetime.
	Enable0RTT bool

	cacheOnce sync.Once
	cache     tls.ClientSessionCache
}

// defaultIdleTimeout is quic-go's default MaxIdleTimeout
// (internal/protocol/params.go).
const defaultIdleTimeout = 30 * time.Second

// Dial establishes a connection. With Enable0RTT it may return before the
// handshake completes; only transport.EarlyConn.OpenStreamEarly can then send data.
func (d *Dialer) Dial(ctx context.Context) (transport.Conn, error) {
	tc, err := configureTLS(d.TLSConfig)
	if err != nil {
		return nil, err
	}
	var qc *quic.Conn
	if d.Enable0RTT {
		if tc.ClientSessionCache == nil {
			d.cacheOnce.Do(func() { d.cache = tls.NewLRUClientSessionCache(1) })
			tc.ClientSessionCache = d.cache
		}
		qc, err = quic.DialAddrEarly(ctx, d.Addr, tc, configureQUIC(d.QUICConfig, true))
	} else {
		qc, err = quic.DialAddr(ctx, d.Addr, tc, configureQUIC(d.QUICConfig, true))
	}
	if err != nil {
		return nil, fmt.Errorf("rpcquic: dial %s: %w", d.Addr, err)
	}
	c := newConn(qc)
	c.client = true
	return c, nil
}

// A server using client certificates or custom TLS authentication must not
// dispatch calls before those checks finish. A dynamic config may enable them.
func needsClientAuthentication(c *tls.Config) bool {
	return c.ClientAuth != tls.NoClientCert || c.VerifyPeerCertificate != nil ||
		c.VerifyConnection != nil || c.GetConfigForClient != nil
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
		c.MaxIncomingUniStreams = 1
		switch {
		case c.KeepAlivePeriod < 0:
			c.KeepAlivePeriod = 0
		case c.KeepAlivePeriod == 0:
			idle := c.MaxIdleTimeout
			if idle <= 0 {
				idle = defaultIdleTimeout
			}
			// quic-go sends keepalives at most every half of the idle
			// timeout agreed with the server (connection.go:2425).
			c.KeepAlivePeriod = idle / 2
		}
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
	qc            *quic.Conn
	ctx           context.Context
	cancel        context.CancelCauseFunc
	client        bool
	waitHandshake bool
}

func newConn(qc *quic.Conn) *conn {
	ctx, cancel := context.WithCancelCause(context.Background())
	qctx := qc.Context()
	context.AfterFunc(qctx, func() { cancel(connError(context.Cause(qctx))) })
	return &conn{qc: qc, ctx: ctx, cancel: cancel}
}

func (c *conn) OpenStream(ctx context.Context) (transport.Stream, error) {
	if err := c.WaitHandshake(ctx); err != nil {
		return nil, err
	}
	return c.OpenStreamEarly(ctx)
}

func (c *conn) OpenStreamEarly(ctx context.Context) (transport.Stream, error) {
	early := false
	select {
	case <-c.qc.HandshakeComplete():
	default:
		early = c.client
	}
	qs, err := c.qc.OpenStreamSync(ctx)
	if errors.Is(err, quic.Err0RTTRejected) {
		// No stream opened, so nothing needs replaying.
		if err := c.WaitHandshake(ctx); err != nil {
			return nil, err
		}
		qs, err = c.qc.OpenStreamSync(ctx)
		early = false
	}
	if err != nil {
		return nil, c.convert(err)
	}
	return newStream(c, qs, early), nil
}

func (c *conn) WaitHandshake(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.qc.Context().Done():
		return c.convert(context.Cause(c.qc.Context()))
	case <-c.qc.HandshakeComplete():
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.qc.Context().Err() != nil {
		return c.convert(context.Cause(c.qc.Context()))
	}
	if c.client {
		// v0.63.0 keeps the same *quic.Conn. NextConnection enables its
		// reset stream maps after rejection; it is a no-op otherwise.
		_, err := c.qc.NextConnection(ctx)
		return c.convert(err)
	}
	return nil
}

func (c *conn) AcceptStream(ctx context.Context) (transport.Stream, error) {
	if c.waitHandshake {
		if err := c.WaitHandshake(ctx); err != nil {
			return nil, err
		}
	}
	qs, err := c.qc.AcceptStream(ctx)
	if err != nil {
		return nil, c.convert(err)
	}
	return newStream(c, qs, false), nil
}

func (c *conn) OpenUniStream(ctx context.Context) (transport.SendStream, error) {
	qs, err := c.qc.OpenUniStreamSync(ctx)
	if err != nil {
		return nil, c.convert(err)
	}
	return &sendStream{c: c, qs: qs}, nil
}

func (c *conn) AcceptUniStream(ctx context.Context) (transport.ReceiveStream, error) {
	// GOAWAY belongs to the authenticated connection, not to the early
	// stream maps invalidated by rejection. Keep its watcher alive.
	if err := c.WaitHandshake(ctx); err != nil {
		return nil, err
	}
	qs, err := c.qc.AcceptUniStream(ctx)
	if err != nil {
		return nil, c.convert(err)
	}
	return &receiveStream{c: c, qs: qs}, nil
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

// quicReceiveStream and quicSendStream are the directions of quic-go's
// streams: *quic.Stream is both, *quic.ReceiveStream and *quic.SendStream
// are one each.
type (
	quicReceiveStream interface {
		Read(b []byte) (int, error)
		CancelRead(code quic.StreamErrorCode)
	}
	quicSendStream interface {
		Write(b []byte) (int, error)
		Close() error
		CancelWrite(code quic.StreamErrorCode)
		Context() context.Context
	}
)

type receiveStream struct {
	c  *conn
	qs quicReceiveStream
}

func (s *receiveStream) Read(b []byte) (int, error) {
	n, err := s.qs.Read(b)
	return n, s.c.convert(err)
}

func (s *receiveStream) CancelRead(code transport.ErrorCode) {
	s.qs.CancelRead(quic.StreamErrorCode(code))
}

type sendStream struct {
	c  *conn
	qs quicSendStream

	ctxOnce    sync.Once
	ctx        context.Context
	early      bool
	closed     atomic.Bool
	cancelOnce sync.Once
}

func (s *sendStream) Write(b []byte) (int, error) {
	n, err := s.qs.Write(b)
	return n, s.c.convert(err)
}

func (s *sendStream) Close() error {
	s.closed.Store(true)
	return s.c.convert(s.qs.Close())
}

func (s *sendStream) CancelWrite(code transport.ErrorCode) {
	if s.early && s.closed.Load() {
		// quic-go v0.63.0 does not set shutdownErr when rejecting a stream
		// already closed with FIN (SendStream.closeForShutdown). CancelWrite
		// on that old stream would send a RESET for an ID reused after
		// NextConnection. Nothing is blocked in Write after Close: defer
		// this reset until acceptance is known, and omit it after rejection.
		s.cancelOnce.Do(func() {
			go func() {
				if s.c.WaitHandshake(s.c.Context()) == nil && s.c.qc.ConnectionState().Used0RTT {
					s.qs.CancelWrite(quic.StreamErrorCode(code))
				}
			}()
		})
		return
	}
	s.qs.CancelWrite(quic.StreamErrorCode(code))
}

type stream struct {
	receiveStream
	sendStream
}

func newStream(c *conn, qs *quic.Stream, early bool) *stream {
	return &stream{receiveStream{c: c, qs: qs}, sendStream{c: c, qs: qs, early: early}}
}

// Context translates the cause of quic-go's stream context. It is built on
// first use: the client side of a call never needs it.
func (s *sendStream) Context() context.Context {
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
	if errors.Is(err, quic.Err0RTTRejected) {
		return transport.ErrEarlyDataRejected
	}
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
	if errors.Is(err, quic.Err0RTTRejected) {
		return transport.ErrEarlyDataRejected
	}
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
