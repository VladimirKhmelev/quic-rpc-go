package rpcquic_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
	rpcquic "github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/quic"
)

type ticketCache struct {
	tls.ClientSessionCache
	ready chan struct{}
	once  sync.Once
}

func (c *ticketCache) Put(key string, state *tls.ClientSessionState) {
	c.ClientSessionCache.Put(key, state)
	if state != nil {
		c.once.Do(func() { close(c.ready) })
	}
}

type earlyDialer struct {
	*rpcquic.Dialer
	last  atomic.Pointer[transport.Conn]
	count atomic.Int32
}

func (d *earlyDialer) Dial(ctx context.Context) (transport.Conn, error) {
	c, err := d.Dialer.Dial(ctx)
	if err == nil {
		d.last.Store(&c)
		d.count.Add(1)
	}
	return c, err
}

type earlyFixture struct {
	d           *earlyDialer
	srv         *rpc.Server
	pc          *transporttest.PacketConn
	cache       *ticketCache
	reject      atomic.Bool
	calls       atomic.Int32
	started     chan string
	desc        rpc.ServiceDesc
	clientCalls atomic.Int32
}

func newEarlyFixture(t testing.TB, delay time.Duration, serverIdempotent bool, editTLS func(*tls.Config)) *earlyFixture {
	t.Helper()
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	f := &earlyFixture{started: make(chan string, 256)}
	serverTLS, clientTLS := transporttest.TLSConfigs(t)
	if editTLS != nil {
		editTLS(serverTLS)
	}
	f.cache = &ticketCache{ClientSessionCache: tls.NewLRUClientSessionCache(1), ready: make(chan struct{})}
	clientTLS.ClientSessionCache = f.cache
	f.pc = transporttest.NewPacketConn(t)
	f.pc.Delay = delay
	tr := &quic.Transport{Conn: f.pc}
	conf := &quic.Config{Allow0RTT: true}
	conf.GetConfigForClient = func(*quic.ClientInfo) (*quic.Config, error) {
		return &quic.Config{Allow0RTT: !f.reject.Load(), MaxIncomingUniStreams: -1}, nil
	}
	l, err := rpcquic.NewListener(tr, serverTLS, conf)
	if err != nil {
		t.Fatal(err)
	}
	f.desc = rpc.ServiceDesc{ServiceName: "early.Echo", Methods: []rpc.MethodDesc{{
		MethodName: "Unary", Idempotent: serverIdempotent,
		Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor rpc.UnaryServerInterceptor) (any, error) {
			in := new(wrapperspb.StringValue)
			if err := dec(in); err != nil {
				return nil, err
			}
			handler := func(context.Context, any) (any, error) { f.started <- in.Value; return in, nil }
			if interceptor != nil {
				return interceptor(ctx, in, &rpc.UnaryServerInfo{Server: srv, FullMethod: "/early.Echo/Unary"}, handler)
			}
			return handler(ctx, in)
		},
	}}}
	for _, kind := range []string{"ServerStream", "ClientStream", "Bidi"} {
		f.desc.Streams = append(f.desc.Streams, rpc.StreamDesc{
			StreamName: kind, Idempotent: serverIdempotent,
			ServerStreams: kind != "ClientStream", ClientStreams: kind != "ServerStream",
			Handler: func(_ any, stream rpc.ServerStream) error {
				in := new(wrapperspb.StringValue)
				if err := stream.RecvMsg(in); err != nil {
					return err
				}
				f.started <- in.Value
				if err := stream.RecvMsg(new(wrapperspb.StringValue)); !errors.Is(err, io.EOF) {
					return err
				}
				return stream.SendMsg(in)
			},
		})
	}
	f.srv = rpc.NewServer(rpc.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *rpc.UnaryServerInfo, next rpc.UnaryHandler) (any, error) {
		f.calls.Add(1)
		return next(ctx, req)
	}))
	f.srv.RegisterService(&f.desc, f)
	served := make(chan error, 1)
	go func() { served <- f.srv.Serve(l) }()
	t.Cleanup(func() {
		f.srv.Stop()
		_ = f.pc.Close()
		_ = tr.Close()
		if err := <-served; err != nil && !errors.Is(err, rpc.ErrServerStopped) {
			t.Errorf("Serve: %v", err)
		}
	})
	f.d = &earlyDialer{Dialer: &rpcquic.Dialer{Addr: l.Addr().String(), TLSConfig: clientTLS, Enable0RTT: true}}
	return f
}

func (f *earlyFixture) client(t testing.TB) *rpc.ClientConn {
	t.Helper()
	cc := rpc.NewClient(f.d, rpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, resp any, cc *rpc.ClientConn, next rpc.UnaryInvoker, opts ...rpc.CallOption) error {
		f.clientCalls.Add(1)
		return next(ctx, method, req, resp, cc, opts...)
	}))
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func earlyCall(ctx context.Context, cc *rpc.ClientConn, kind, value string, opts ...rpc.CallOption) error {
	in, out := wrapperspb.String(value), new(wrapperspb.StringValue)
	if kind == "Unary" {
		if err := cc.Invoke(ctx, "/early.Echo/Unary", in, out, opts...); err != nil {
			return err
		}
	} else {
		desc := &rpc.StreamDesc{ServerStreams: kind != "ClientStream", ClientStreams: kind != "ServerStream"}
		st, err := cc.NewStream(ctx, desc, "/early.Echo/"+kind, opts...)
		if err != nil {
			return err
		}
		_ = st.SendMsg(in) // RecvMsg reports the precise status after a failed send.
		_ = st.CloseSend()
		if err := st.RecvMsg(out); err != nil {
			return err
		}
		if desc.ServerStreams {
			if err := st.RecvMsg(new(wrapperspb.StringValue)); !errors.Is(err, io.EOF) {
				return fmt.Errorf("end of stream: %w", err)
			}
		}
	}
	if out.Value != value {
		return fmt.Errorf("response %q, want %q", out.Value, value)
	}
	return nil
}

func (f *earlyFixture) warm(ctx context.Context, t testing.TB) {
	t.Helper()
	cc := f.client(t)
	if err := earlyCall(ctx, cc, "Unary", "warm"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.cache.ready:
	case <-ctx.Done():
		t.Fatal("no TLS ticket")
	}
	<-f.started
	_ = cc.Close()
}

func await[T any](ctx context.Context, t testing.TB, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		var zero T
		return zero
	}
}

// With server packets held, neither endpoint can finish the resumed handshake.
// Nevertheless every marked RPC kind reaches its handler using real 0-RTT.
func TestEarlyCalls(t *testing.T) {
	for _, kind := range []string{"Unary", "ServerStream", "ClientStream", "Bidi"} {
		t.Run(kind, func(t *testing.T) {
			ctx := transporttest.Context(t)
			f := newEarlyFixture(t, 0, true, nil)
			f.warm(ctx, t)
			_, release := f.pc.HoldWrites()
			defer release()
			cc := f.client(t)
			done := make(chan error, 1)
			go func() { done <- earlyCall(ctx, cc, kind, "early", rpc.Idempotent(true)) }()
			if got := await(ctx, t, f.started); got != "early" {
				t.Fatalf("handler: %s", got)
			}
			state := rpcquic.StateForTest(*f.d.last.Load())
			if state.TLS.HandshakeComplete {
				t.Fatal("handshake finished through held packets")
			}
			release()
			if err := await(ctx, t, done); err != nil {
				t.Fatal(err)
			}
			state = rpcquic.StateForTest(*f.d.last.Load())
			if !state.Used0RTT || !state.TLS.DidResume {
				t.Fatalf("not a resumed 0-RTT connection: %+v", state)
			}
		})
	}
}

func TestEarlyRejection(t *testing.T) {
	for _, kind := range []string{"Unary", "ServerStream", "ClientStream", "Bidi"} {
		t.Run(kind, func(t *testing.T) {
			ctx := transporttest.Context(t)
			f := newEarlyFixture(t, 0, true, nil)
			f.warm(ctx, t)
			f.reject.Store(true)
			cc := f.client(t)
			err := earlyCall(ctx, cc, kind, "rejected", rpc.Idempotent(true))
			if kind == "Unary" {
				if err != nil {
					t.Fatal(err)
				}
				if f.calls.Load() != 2 || f.clientCalls.Load() != 2 {
					t.Fatal("retry repeated an interceptor or handler")
				}
			} else if status.Code(err) != codes.Unavailable {
				t.Fatalf("rejected stream: %v", err)
			}
			if err := earlyCall(ctx, cc, "Unary", "after rejection"); err != nil {
				t.Fatal(err)
			}
			if f.d.count.Load() != 2 {
				t.Fatal("rejection caused a needless redial")
			}
			state := rpcquic.StateForTest(*f.d.last.Load())
			if state.Used0RTT || !state.TLS.DidResume {
				t.Fatal("expected resumed connection with rejected early data")
			}
			// The GOAWAY watcher must have survived the rejection.
			stopped := make(chan struct{})
			go func() { f.srv.GracefulStop(ctx); close(stopped) }()
			await(ctx, t, stopped)
			if ctx.Err() != nil {
				t.Fatal("graceful stop timed out after rejection")
			}
		})
	}
}

func TestEarlyWaitCancellation(t *testing.T) {
	for _, serverIdempotent := range []bool{false, true} {
		t.Run(fmt.Sprint(serverIdempotent), func(t *testing.T) {
			ctx := transporttest.Context(t)
			f := newEarlyFixture(t, 0, serverIdempotent, nil)
			f.warm(ctx, t)
			ready, release := f.pc.HoldWrites()
			defer release()
			cc := f.client(t)
			callCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			// Test both the server guard and the client's ordinary-call guard.
			go func() { done <- earlyCall(callCtx, cc, "Unary", "canceled", rpc.Idempotent(!serverIdempotent)) }()
			await(ctx, t, ready)
			cancel()
			if err := await(ctx, t, done); status.Code(err) != codes.Canceled {
				t.Fatalf("cancellation: %v", err)
			}
			if f.calls.Load() != 1 {
				t.Fatal("non-idempotent handler ran before authentication")
			}
			release()
			if err := earlyCall(ctx, cc, "Unary", "after cancellation"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A custom TLS authentication failure must happen before any RPC effects,
// even for a method whose application semantics are idempotent.
func TestEarlyAuthenticationFailure(t *testing.T) {
	var reject atomic.Bool
	f := newEarlyFixture(t, 0, true, func(c *tls.Config) {
		c.VerifyConnection = func(tls.ConnectionState) error {
			if reject.Load() {
				return errors.New("credentials revoked")
			}
			return nil
		}
	})
	ctx := transporttest.Context(t)
	f.warm(ctx, t)
	reject.Store(true)
	cc := f.client(t)
	if err := earlyCall(ctx, cc, "Unary", "unauthenticated", rpc.Idempotent(true)); status.Code(err) != codes.Unavailable {
		t.Fatalf("authentication: %v", err)
	}
	if f.calls.Load() != 1 {
		t.Fatal("handler ran before TLS authentication failed")
	}
}

func TestEarlyConcurrentRejection(t *testing.T) {
	ctx := transporttest.Context(t)
	f := newEarlyFixture(t, 0, true, nil)
	f.warm(ctx, t)
	f.reject.Store(true)
	ready, release := f.pc.HoldWrites()
	defer release()
	cc := f.client(t)
	const n = 32
	done := make(chan error, n)
	for i := range n {
		go func() { done <- earlyCall(ctx, cc, "Unary", fmt.Sprint(i), rpc.Idempotent(i%2 == 0)) }()
	}
	await(ctx, t, ready)
	release()
	for range n {
		if err := await(ctx, t, done); err != nil {
			t.Fatal(err)
		}
	}
	if f.calls.Load() != n+1 || f.clientCalls.Load() != n+1 {
		t.Fatal("concurrent retries duplicated calls")
	}
	if f.d.count.Load() != 2 {
		t.Fatal("concurrent calls did not share the connection")
	}
}

func TestEarlyDefaultCache(t *testing.T) {
	ctx := transporttest.Context(t)
	f := newEarlyFixture(t, 0, true, nil)
	f.d.TLSConfig.ClientSessionCache = nil
	cc := f.client(t)
	if err := earlyCall(ctx, cc, "Unary", "first", rpc.Idempotent(true)); err != nil {
		t.Fatal(err)
	}
	state := rpcquic.StateForTest(*f.d.last.Load())
	if state.Used0RTT || state.TLS.DidResume {
		t.Fatal("first connection unexpectedly resumed")
	}
	cache := rpcquic.CacheForTest(f.d.Dialer)
	if cache == nil || f.d.TLSConfig.ClientSessionCache != nil {
		t.Fatal("cache missing or caller's TLS config modified")
	}
	// TLS tickets arrive asynchronously after the initial handshake.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, ok := cache.Get("localhost"); ok {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("no session ticket")
		}
	}
	_ = cc.Close()
	cc = f.client(t)
	if err := earlyCall(ctx, cc, "Unary", "second", rpc.Idempotent(true)); err != nil {
		t.Fatal(err)
	}
	if !rpcquic.StateForTest(*f.d.last.Load()).Used0RTT {
		t.Fatal("dialer did not reuse its cached session")
	}
}

func TestEarlyDisabled(t *testing.T) {
	for _, side := range []string{"client", "server"} {
		t.Run(side, func(t *testing.T) {
			ctx := transporttest.Context(t)
			f := newEarlyFixture(t, 0, true, nil)
			if side == "client" {
				f.d.Enable0RTT = false
			} else {
				f.reject.Store(true)
			}
			f.warm(ctx, t)
			cc := f.client(t)
			if err := earlyCall(ctx, cc, "Unary", "ordinary", rpc.Idempotent(true)); err != nil {
				t.Fatal(err)
			}
			state := rpcquic.StateForTest(*f.d.last.Load())
			if state.Used0RTT || !state.TLS.DidResume {
				t.Fatal("expected ordinary TLS resumption")
			}
		})
	}
}

// Compare resumed TLS in both cases; only early application data differs.
// The controlled server-to-client delay adds one RTT without serially delaying
// every datagram. The measured interval includes dialing and the first reply.
func BenchmarkEarlyReconnect(b *testing.B) {
	for _, early := range []bool{false, true} {
		b.Run(fmt.Sprintf("0RTT=%t", early), func(b *testing.B) {
			const rtt = 50 * time.Millisecond
			ctx, cancel := context.WithTimeout(b.Context(), 2*time.Minute)
			defer cancel()
			f := newEarlyFixture(b, rtt, true, nil)
			f.warm(ctx, b)
			f.d = &earlyDialer{Dialer: &rpcquic.Dialer{Addr: f.d.Addr, TLSConfig: f.d.TLSConfig, Enable0RTT: early}}
			var elapsed time.Duration
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				cc := rpc.NewClient(f.d)
				start := time.Now()
				err := earlyCall(ctx, cc, "Unary", "measured", rpc.Idempotent(true))
				elapsed += time.Since(start)
				if err != nil {
					b.Fatal(err)
				}
				state := rpcquic.StateForTest(*f.d.last.Load())
				if !state.TLS.DidResume || state.Used0RTT != early {
					b.Fatal("unexpected handshake mode")
				}
				<-f.started
				_ = cc.Close()
			}
			b.StopTimer()
			b.ReportMetric(float64(elapsed)/float64(b.N)/float64(time.Millisecond), "first_reply_ms")
			b.ReportMetric(float64(elapsed)/float64(b.N)/float64(rtt), "RTT/reply")
		})
	}
}
