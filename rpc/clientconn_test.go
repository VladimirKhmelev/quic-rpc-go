package rpc_test

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
	rpcquic "github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/quic"
)

// switchDialer lets a test point a ClientConn at another server.
type switchDialer struct {
	target atomic.Pointer[transport.Dialer]
}

func (s *switchDialer) Dial(ctx context.Context) (transport.Conn, error) {
	return (*s.target.Load()).Dial(ctx)
}

// countingDialer dials with d, counts the dials and remembers the last
// connection. If wrap is set, it hands out wrap of each connection instead.
type countingDialer struct {
	d     transport.Dialer
	wrap  func(transport.Conn) transport.Conn
	dials atomic.Int32
	last  transport.Conn // written by Dial, which runs on the calling goroutine
}

func (c *countingDialer) Dial(ctx context.Context) (transport.Conn, error) {
	c.dials.Add(1)
	conn, err := c.d.Dial(ctx)
	if err != nil {
		return nil, err
	}
	c.last = conn
	if c.wrap != nil {
		return c.wrap(conn), nil
	}
	return conn, nil
}

// staleConn hides for a while that its connection has closed, as the QUIC
// transport may: quic-go fails the streams of a closed connection before it
// cancels the connection's context. Its own context is canceled once
// OpenStream reports the connection closed, as transport.Conn requires.
type staleConn struct {
	transport.Conn
	ctx    context.Context
	cancel context.CancelFunc
}

func newStaleConn(c transport.Conn) transport.Conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &staleConn{Conn: c, ctx: ctx, cancel: cancel}
}

func (c *staleConn) Context() context.Context { return c.ctx }

func (c *staleConn) OpenStream(ctx context.Context) (transport.Stream, error) {
	st, err := c.Conn.OpenStream(ctx)
	if _, closed := errors.AsType[*transport.ConnError](err); closed {
		c.cancel()
	}
	return st, err
}

// serveEcho serves the test service on a fresh transport.
func serveEcho(t *testing.T, f transporttest.Factory) (*rpc.Server, transport.Dialer, chan error) {
	t.Helper()
	l, d := f(t, 10)
	srv := rpc.NewServer()
	srv.RegisterService(&echoDesc, &testServer{})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	return srv, d, served
}

// waitClosed waits until c reports itself closed.
func waitClosed(t *testing.T, c transport.Conn) {
	t.Helper()
	select {
	case <-c.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the client did not notice that the server closed the connection")
	}
}

// TestRedialAfterConnectionLoss: once the client has noticed that the
// server closed the connection, the next call dials again.
func TestRedialAfterConnectionLoss(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		first, d1, served1 := serveEcho(t, f)
		var target switchDialer
		target.target.Store(&d1)
		dialer := &countingDialer{d: &target}
		cc := rpc.NewClient(dialer)
		defer func() { _ = cc.Close() }()

		ctx := transporttest.Context(t)
		if err := cc.Invoke(ctx, unaryMethod, wrapperspb.String("1"), new(msg)); err != nil {
			t.Fatalf("first call: %v", err)
		}
		first.Stop()
		<-served1
		waitClosed(t, dialer.last)

		second, d2, served2 := serveEcho(t, f)
		defer func() { second.Stop(); <-served2 }()
		target.target.Store(&d2)
		if err := cc.Invoke(ctx, unaryMethod, wrapperspb.String("2"), new(msg)); err != nil {
			t.Fatalf("call after the server came back: %v", err)
		}
	})
}

// TestRetryOnClosedConnection: the server has closed the connection, but
// the client has not noticed yet. The first call fails to open a stream on
// it, has sent nothing, and goes out on a new connection (RPC-12).
func TestRetryOnClosedConnection(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		first, d1, served1 := serveEcho(t, f)
		var target switchDialer
		target.target.Store(&d1)
		dialer := &countingDialer{d: &target, wrap: newStaleConn}
		cc := rpc.NewClient(dialer)
		defer func() { _ = cc.Close() }()

		ctx := transporttest.Context(t)
		if err := cc.Invoke(ctx, unaryMethod, wrapperspb.String("1"), new(msg)); err != nil {
			t.Fatalf("first call: %v", err)
		}
		first.Stop()
		<-served1
		waitClosed(t, dialer.last)

		second, d2, served2 := serveEcho(t, f)
		defer func() { second.Stop(); <-served2 }()
		target.target.Store(&d2)
		if err := cc.Invoke(ctx, unaryMethod, wrapperspb.String("2"), new(msg)); err != nil {
			t.Fatalf("first call after the server closed the connection: %v", err)
		}
		if n := dialer.dials.Load(); n != 2 {
			t.Errorf("dialed %d times, want 2", n)
		}
	})
}

// countingConn counts the streams it was asked to open.
type countingConn struct {
	transport.Conn
	opens *atomic.Int32
}

func (c *countingConn) OpenStream(ctx context.Context) (transport.Stream, error) {
	c.opens.Add(1)
	return c.Conn.OpenStream(ctx)
}

// brokenConn fails to open streams.
type brokenConn struct{ transport.Conn }

func (c *brokenConn) OpenStream(context.Context) (transport.Stream, error) {
	return nil, errors.New("no stream")
}

// TestRetryOnce: a call retries a stream that failed to open once, and only
// if the connection closed.
func TestRetryOnce(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		for _, tt := range []struct {
			name         string
			wrap         func(transport.Conn) transport.Conn
			dials, opens int32
		}{
			{"every connection closed", func(c transport.Conn) transport.Conn {
				_ = c.CloseWithError(transport.NoError, "")
				return newStaleConn(c)
			}, 2, 2},
			{"another error", func(c transport.Conn) transport.Conn { return &brokenConn{c} }, 1, 1},
		} {
			t.Run(tt.name, func(t *testing.T) {
				srv, d, served := serveEcho(t, f)
				defer func() { srv.Stop(); <-served }()
				var opens atomic.Int32
				dialer := &countingDialer{d: d, wrap: func(c transport.Conn) transport.Conn {
					return &countingConn{Conn: tt.wrap(c), opens: &opens}
				}}
				cc := rpc.NewClient(dialer)
				defer func() { _ = cc.Close() }()
				err := cc.Invoke(transporttest.Context(t), unaryMethod, wrapperspb.String("1"), new(msg))
				wantCode(t, err, codes.Unavailable, "call")
				if n := dialer.dials.Load(); n != tt.dials {
					t.Errorf("dialed %d times, want %d", n, tt.dials)
				}
				if n := opens.Load(); n != tt.opens {
					t.Errorf("opened %d streams, want %d", n, tt.opens)
				}
			})
		}
	})
}

// TestServerRestart crashes a QUIC server and starts it again on the same
// port with the same stateless reset key. The client is not told of the
// crash: its first call goes out on the old connection and fails with
// Unavailable as soon as the new server answers it with a reset, not after
// the idle timeout; the next call dials again.
func TestServerRestart(t *testing.T) {
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	serverTLS, clientTLS := transporttest.TLSConfigs(t)
	key := quic.StatelessResetKey([]byte("a key that outlives the server.."))
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	start := func() (*quic.Transport, chan error) {
		pc, err := net.ListenUDP("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		addr = pc.LocalAddr().(*net.UDPAddr) //nolint:errcheck // a UDP socket has a UDP address
		tr := &quic.Transport{Conn: pc, StatelessResetKey: &key}
		l, err := rpcquic.NewListener(tr, serverTLS, nil)
		if err != nil {
			t.Fatal(err)
		}
		srv := rpc.NewServer()
		srv.RegisterService(&echoDesc, &testServer{})
		served := make(chan error, 1)
		go func() { served <- srv.Serve(l) }()
		t.Cleanup(func() { srv.Stop(); _ = tr.Close(); _ = pc.Close() })
		return tr, served
	}
	tr, served := start()
	cc := rpc.NewClient(&rpcquic.Dialer{Addr: addr.String(), TLSConfig: clientTLS})
	defer func() { _ = cc.Close() }()
	ctx := transporttest.Context(t)
	if err := cc.Invoke(ctx, unaryMethod, wrapperspb.String("1"), new(msg)); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// As a killed process: the connections vanish without a word.
	_ = tr.Close()
	_ = tr.Conn.Close()
	<-served
	start()
	began := time.Now()
	err := cc.Invoke(ctx, unaryMethod, wrapperspb.String("2"), new(msg))
	wantCode(t, err, codes.Unavailable, "call on the connection to the crashed server")
	if d := time.Since(began); d > time.Second {
		t.Errorf("the call failed after %v", d)
	}
	if err := cc.Invoke(ctx, unaryMethod, wrapperspb.String("3"), new(msg)); err != nil {
		t.Fatalf("call after the restart: %v", err)
	}
}

// TestDeadServer: a server dies without a word and without a stateless
// reset key, so nothing tells the client. The idle timeout finds the
// connection dead: a call in flight fails with Unavailable, an idle
// connection closes as well, and the next call dials the server that came
// back (RPC-12).
func TestDeadServer(t *testing.T) {
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	serverTLS, clientTLS := transporttest.TLSConfigs(t)
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	hanging := make(chan struct{}, 1)
	impl := &testServer{unary: func(ctx context.Context, in *msg) (*msg, error) {
		if in.GetValue() == "hang" {
			hanging <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return in, nil
	}}
	start := func() *quic.Transport {
		pc, err := net.ListenUDP("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		addr = pc.LocalAddr().(*net.UDPAddr) //nolint:errcheck // a UDP socket has a UDP address
		tr := &quic.Transport{Conn: pc}
		l, err := rpcquic.NewListener(tr, serverTLS, nil)
		if err != nil {
			t.Fatal(err)
		}
		srv := rpc.NewServer()
		srv.RegisterService(&echoDesc, impl)
		go func() { _ = srv.Serve(l) }()
		t.Cleanup(func() { srv.Stop(); _ = tr.Close(); _ = pc.Close() })
		return tr
	}
	// As a killed process: the connections vanish without a word.
	crash := func(tr *quic.Transport) {
		_ = tr.Close()
		_ = tr.Conn.Close()
	}
	tr := start()
	const idle = 300 * time.Millisecond
	dialer := &countingDialer{d: &rpcquic.Dialer{Addr: addr.String(), TLSConfig: clientTLS, QUICConfig: &quic.Config{MaxIdleTimeout: idle}}}
	cc := rpc.NewClient(dialer)
	defer func() { _ = cc.Close() }()
	ctx := transporttest.Context(t)
	call := func(ctx context.Context, in string) error {
		return cc.Invoke(ctx, unaryMethod, wrapperspb.String(in), new(msg))
	}

	result := make(chan error, 1)
	go func() { result <- call(ctx, "hang") }()
	<-hanging
	crash(tr)
	began := time.Now()
	wantCode(t, <-result, codes.Unavailable, "call in flight when the server died")
	if d := time.Since(began); d > 10*idle {
		t.Errorf("the call failed %v after the server died, the idle timeout is %v", d, idle)
	}

	tr = start()
	if err := call(ctx, "1"); err != nil {
		t.Fatalf("call after the server came back: %v", err)
	}
	crash(tr)
	waitClosed(t, dialer.last)
	start()
	if err := call(ctx, "2"); err != nil {
		t.Fatalf("call after the server came back again: %v", err)
	}
	if n := dialer.dials.Load(); n != 3 {
		t.Errorf("dialed %d times, want 3", n)
	}
}

func TestClientConnClose(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		started := make(chan struct{})
		e := newEnv(t, f, &testServer{unary: func(ctx context.Context, _ *msg) (*msg, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}})
		result := make(chan error, 1)
		go func() {
			_, err := e.unary(transporttest.Context(t), "x")
			result <- err
		}()
		<-started
		if err := e.cc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		wantCode(t, <-result, codes.Canceled, "call in flight during Close")
		_, err := e.unary(transporttest.Context(t), "x")
		wantCode(t, err, codes.Canceled, "call after Close")
		if err := e.cc.Close(); err == nil {
			t.Fatal("second Close succeeded")
		}
	})
}
