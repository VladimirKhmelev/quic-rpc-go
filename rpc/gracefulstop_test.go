package rpc_test

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// stoppable is a server and a client for the tests of GracefulStop. The
// client dials through a switchDialer, so that a test can bring up another
// server for the calls that leave the stopping one.
type stoppable struct {
	srv    *rpc.Server
	l      transport.Listener
	d      transport.Dialer
	dialer *countingDialer
	target switchDialer
	cc     *rpc.ClientConn
	served chan error
}

func newStoppable(t *testing.T, f transporttest.Factory, impl *testServer) *stoppable {
	t.Helper()
	l, d := f(t, 10)
	return startStoppable(t, l, d, impl, nil)
}

// startStoppable serves impl on l; wrap, if set, wraps the client's
// connections.
func startStoppable(t *testing.T, l transport.Listener, d transport.Dialer, impl *testServer, wrap func(transport.Conn) transport.Conn) *stoppable {
	t.Helper()
	s := &stoppable{srv: rpc.NewServer(), l: l, d: d, served: make(chan error, 1)}
	s.target.target.Store(&d)
	s.dialer = &countingDialer{d: &s.target, wrap: wrap}
	s.cc = rpc.NewClient(s.dialer)
	s.srv.RegisterService(&echoDesc, impl)
	go func() { s.served <- s.srv.Serve(l) }()
	t.Cleanup(func() { _ = s.cc.Close(); s.srv.Stop() })
	return s
}

// moveTo points the client at a new server with the default echo service.
func (s *stoppable) moveTo(t *testing.T, f transporttest.Factory) {
	t.Helper()
	srv, d, served := serveEcho(t, f)
	t.Cleanup(func() { srv.Stop(); <-served })
	s.target.target.Store(&d)
}

// call starts a call and returns where its result arrives.
func (s *stoppable) call(ctx context.Context) <-chan error {
	result := make(chan error, 1)
	go func() { result <- s.cc.Invoke(ctx, unaryMethod, wrapperspb.String("x"), new(msg)) }()
	return result
}

// gracefulStop starts GracefulStop and returns where the time it took
// arrives.
func (s *stoppable) gracefulStop(ctx context.Context) <-chan time.Duration {
	took := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		s.srv.GracefulStop(ctx)
		took <- time.Since(start)
	}()
	return took
}

func sleepingHandler(d time.Duration) *testServer {
	return &testServer{unary: func(_ context.Context, in *msg) (*msg, error) {
		time.Sleep(d)
		return in, nil
	}}
}

func blockingHandler() *testServer {
	return &testServer{unary: func(ctx context.Context, _ *msg) (*msg, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
}

// TestGracefulStop: the listener closes at once and Serve returns nil; a
// call made after GOAWAY goes out on a new connection, to another server,
// while the call in flight finishes on the old one; the client then closes
// the old connection, and GracefulStop returns.
func TestGracefulStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStoppable(t, transporttest.Mem, sleepingHandler(time.Second))
		first := s.call(t.Context())
		time.Sleep(100 * time.Millisecond)
		took := s.gracefulStop(t.Context())
		if err := <-s.served; err != nil {
			t.Fatalf("Serve during GracefulStop: %v", err)
		}
		if _, err := s.d.Dial(t.Context()); !errors.Is(err, transport.ErrListenerClosed) {
			t.Errorf("Dial during GracefulStop: %v", err)
		}
		// Nor does the server take a new listener.
		if l, _ := transporttest.Mem(t, 10); !errors.Is(s.srv.Serve(l), rpc.ErrServerStopped) {
			t.Error("Serve during GracefulStop did not fail")
		}
		synctest.Wait() // GOAWAY has arrived
		s.moveTo(t, transporttest.Mem)
		if err := <-s.call(t.Context()); err != nil {
			t.Errorf("call after GOAWAY: %v", err)
		}
		if err := <-first; err != nil {
			t.Errorf("call in flight: %v", err)
		}
		if d := <-took; d != 900*time.Millisecond {
			t.Errorf("GracefulStop took %v, want 0.9 s", d)
		}
		if n := s.dialer.dials.Load(); n != 2 {
			t.Errorf("dialed %d times, want 2", n)
		}
		if err := s.srv.Serve(s.l); !errors.Is(err, rpc.ErrServerStopped) {
			t.Errorf("Serve after GracefulStop: %v", err)
		}
	})
}

// TestGracefulStopIdle: a client without calls closes its connection as
// soon as GOAWAY arrives, so GracefulStop returns at once.
func TestGracefulStopIdle(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStoppable(t, transporttest.Mem, sleepingHandler(0))
		if err := <-s.call(t.Context()); err != nil {
			t.Fatal(err)
		}
		if d := <-s.gracefulStop(t.Context()); d != 0 {
			t.Errorf("GracefulStop took %v", d)
		}
	})
}

// TestGracefulStopTimeout: a call that does not finish before ctx ends fails
// with Unavailable when GracefulStop gives up.
func TestGracefulStopTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStoppable(t, transporttest.Mem, blockingHandler())
		result := s.call(t.Context())
		synctest.Wait()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if d := <-s.gracefulStop(ctx); d != 2*time.Second {
			t.Errorf("GracefulStop took %v, want 2 s", d)
		}
		wantCode(t, <-result, codes.Unavailable, "call in flight when GracefulStop gave up")
	})
}

// TestStopDuringGracefulStop: Stop ends the wait at once, even for a
// handler that ignores its canceled context.
func TestStopDuringGracefulStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStoppable(t, transporttest.Mem, sleepingHandler(time.Minute))
		result := s.call(t.Context())
		synctest.Wait()
		took := s.gracefulStop(t.Context())
		time.Sleep(time.Second)
		s.srv.Stop()
		if d := <-took; d != time.Second {
			t.Errorf("GracefulStop took %v, want 1 s", d)
		}
		wantCode(t, <-result, codes.Unavailable, "call in flight during Stop")
		// The handler ends in its own time; the bubble waits for it.
		time.Sleep(time.Minute)
	})
}

// TestGracefulStopWaitsForHandlers: the client may close its connection
// while a handler still runs, as after canceling the call; GracefulStop
// waits for the handler too.
func TestGracefulStopWaitsForHandlers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStoppable(t, transporttest.Mem, sleepingHandler(time.Second))
		ctx, cancel := context.WithCancel(t.Context())
		result := s.call(ctx)
		synctest.Wait()
		took := s.gracefulStop(t.Context())
		cancel()
		wantCode(t, <-result, codes.Canceled, "canceled call")
		if d := <-took; d != time.Second {
			t.Errorf("GracefulStop took %v, want 1 s", d)
		}
	})
}

// TestGracefulStopStream: a streaming call in flight during GOAWAY goes on
// as usual and keeps the connection until it ends; a streaming call whose
// context is canceled gives the connection up as well.
func TestGracefulStopStream(t *testing.T) {
	for _, tt := range []struct {
		name string
		end  func(t *testing.T, stream rpc.ClientStream, cancel context.CancelFunc)
	}{
		{"ends", func(t *testing.T, stream rpc.ClientStream, _ context.CancelFunc) {
			if err := stream.CloseSend(); err != nil {
				t.Fatal(err)
			}
			if err := stream.RecvMsg(new(msg)); !errors.Is(err, io.EOF) {
				t.Fatalf("Recv after CloseSend: %v", err)
			}
		}},
		{"canceled", func(_ *testing.T, _ rpc.ClientStream, cancel context.CancelFunc) { cancel() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := newStoppable(t, transporttest.Mem, &testServer{})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				stream, err := s.cc.NewStream(ctx, &echoDesc.Streams[0], "/test.v1.Echo/Bidi")
				if err != nil {
					t.Fatal(err)
				}
				synctest.Wait() // the server has accepted the stream
				took := s.gracefulStop(t.Context())
				time.Sleep(time.Second)
				for _, v := range []string{"a", "b"} {
					if err := stream.SendMsg(wrapperspb.String(v)); err != nil {
						t.Fatalf("Send after GOAWAY: %v", err)
					}
					got := new(msg)
					if err := stream.RecvMsg(got); err != nil || got.GetValue() != v {
						t.Fatalf("Recv after GOAWAY: %q, %v", got.GetValue(), err)
					}
				}
				tt.end(t, stream, cancel)
				if d := <-took; d != time.Second {
					t.Errorf("GracefulStop took %v, want 1 s", d)
				}
			})
		})
	}
}

// deafConn never hears GOAWAY, as if it was still on its way.
type deafConn struct{ transport.Conn }

func (c *deafConn) AcceptUniStream(ctx context.Context) (transport.ReceiveStream, error) {
	<-ctx.Done()
	return nil, context.Cause(ctx)
}

// TestGracefulStopRefuses: a call opened before GOAWAY reached the client
// is refused unprocessed. A unary call goes out once more on a new
// connection, to another server; a streaming call fails with Unavailable.
// Either way the client takes the old connection out of use and closes it
// once the call in flight there is done.
func TestGracefulStopRefuses(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		for _, tt := range []struct {
			name  string
			call  func(ctx context.Context, cc *rpc.ClientConn) error
			code  codes.Code
			dials int32
		}{
			{"unary", func(ctx context.Context, cc *rpc.ClientConn) error {
				return cc.Invoke(ctx, unaryMethod, wrapperspb.String("y"), new(msg))
			}, codes.OK, 2},
			{"streaming", func(ctx context.Context, cc *rpc.ClientConn) error {
				stream, err := cc.NewStream(ctx, &echoDesc.Streams[0], "/test.v1.Echo/Bidi")
				if err != nil {
					return err
				}
				return stream.RecvMsg(new(msg))
			}, codes.Unavailable, 1},
		} {
			t.Run(tt.name, func(t *testing.T) {
				var handled atomic.Int32
				release := make(chan struct{})
				impl := &testServer{unary: func(_ context.Context, in *msg) (*msg, error) {
					handled.Add(1)
					<-release
					return in, nil
				}}
				l, d := f(t, 10)
				s := startStoppable(t, l, d, impl, func(c transport.Conn) transport.Conn { return &deafConn{c} })
				ctx := transporttest.Context(t)
				first := s.call(ctx)
				for handled.Load() == 0 {
					time.Sleep(time.Millisecond)
				}
				took := s.gracefulStop(ctx)
				// Serve returns once the listener is closed, after the
				// connection was marked as going away.
				if err := <-s.served; err != nil {
					t.Fatalf("Serve during GracefulStop: %v", err)
				}
				s.moveTo(t, f)
				wantCode(t, tt.call(ctx, s.cc), tt.code, "call opened before GOAWAY arrived")
				if n := handled.Load(); n != 1 {
					t.Errorf("the stopping server handled %d calls, want 1", n)
				}
				if n := s.dialer.dials.Load(); n != tt.dials {
					t.Errorf("dialed %d times, want %d", n, tt.dials)
				}
				close(release)
				if err := <-first; err != nil {
					t.Errorf("call in flight: %v", err)
				}
				if d := <-took; d > 5*time.Second {
					t.Errorf("GracefulStop took %v", d)
				}
			})
		}
	})
}

// TestGracefulStopDeafClient: GracefulStop waits for the client to close
// its connection, even one without calls; a client that never hears GOAWAY
// holds it up until ctx ends.
func TestGracefulStopDeafClient(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, d := transporttest.Mem(t, 10)
		s := startStoppable(t, l, d, &testServer{}, func(c transport.Conn) transport.Conn { return &deafConn{c} })
		if err := <-s.call(t.Context()); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if d := <-s.gracefulStop(ctx); d != 2*time.Second {
			t.Errorf("GracefulStop took %v, want 2 s", d)
		}
	})
}

// TestCloseAfterGoAway: Close closes the connections that are going away
// too; their calls fail with Canceled.
func TestCloseAfterGoAway(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStoppable(t, transporttest.Mem, blockingHandler())
		result := s.call(t.Context())
		synctest.Wait()
		took := s.gracefulStop(t.Context())
		synctest.Wait() // GOAWAY has arrived
		if err := s.cc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		wantCode(t, <-result, codes.Canceled, "call in flight during Close")
		if d := <-took; d != 0 {
			t.Errorf("GracefulStop took %v", d)
		}
	})
}

// TestGracefulStopAfterFailedCalls: calls that fail before they are sent
// give their connection up, so the client still closes it after GOAWAY.
func TestGracefulStopAfterFailedCalls(t *testing.T) {
	for _, tt := range []struct {
		name  string
		limit int
		call  func(t *testing.T, s *stoppable)
	}{
		{"not a protobuf message", 10, func(t *testing.T, s *stoppable) {
			err := s.cc.Invoke(t.Context(), unaryMethod, "not a message", new(msg))
			wantCode(t, err, codes.Internal, "call with a bad request")
		}},
		{"no stream before the deadline", 1, func(t *testing.T, s *stoppable) {
			// The stream limit is 1, and the stream is taken.
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			wantCode(t, <-s.call(ctx), codes.DeadlineExceeded, "call waiting for a stream")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l, d := transporttest.Mem(t, tt.limit)
				release := make(chan struct{})
				s := startStoppable(t, l, d, &testServer{unary: func(_ context.Context, in *msg) (*msg, error) {
					<-release
					return in, nil
				}}, nil)
				first := s.call(t.Context())
				synctest.Wait()
				tt.call(t, s)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				took := s.gracefulStop(ctx)
				synctest.Wait()
				close(release)
				if err := <-first; err != nil {
					t.Errorf("call in flight: %v", err)
				}
				if d := <-took; d != 0 {
					t.Errorf("GracefulStop took %v", d)
				}
			})
		})
	}
}

// TestGracefulStopTwice: a second GracefulStop sends no second GOAWAY.
func TestGracefulStopTwice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := newStoppable(t, transporttest.Mem, blockingHandler())
		result := s.call(t.Context())
		synctest.Wait()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		first, second := s.gracefulStop(ctx), s.gracefulStop(ctx)
		synctest.Wait() // the client has read GOAWAY
		wait, stop := context.WithTimeout(t.Context(), time.Second)
		defer stop()
		if st, err := s.dialer.last.AcceptUniStream(wait); err == nil {
			t.Errorf("the server sent GOAWAY again, on %v", st)
		}
		<-first
		<-second
		wantCode(t, <-result, codes.Unavailable, "call in flight when GracefulStop gave up")
	})
}

// gatedListener accepts a connection per token in admit.
type gatedListener struct {
	transport.Listener
	admit chan struct{}
}

func (l *gatedListener) Accept(ctx context.Context) (transport.Conn, error) {
	select {
	case <-l.admit:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return l.Listener.Accept(ctx)
}

// TestGracefulStopLateConnection: a connection dialed before GracefulStop
// but accepted during it gets GOAWAY at once, and its call is refused and
// sent elsewhere.
func TestGracefulStopLateConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		impl := &testServer{unary: func(_ context.Context, in *msg) (*msg, error) {
			if in.GetValue() == "slow" {
				<-release
			}
			return in, nil
		}}
		l, d := transporttest.Mem(t, 10)
		gated := &gatedListener{Listener: l, admit: make(chan struct{}, 2)}
		gated.admit <- struct{}{}
		s := startStoppable(t, gated, d, impl, nil)
		slow := make(chan error, 1)
		go func() { slow <- s.cc.Invoke(t.Context(), unaryMethod, wrapperspb.String("slow"), new(msg)) }()
		synctest.Wait() // the slow call keeps the server stopping

		var target switchDialer
		target.target.Store(&d)
		dialer := &countingDialer{d: &target}
		late := rpc.NewClient(dialer)
		defer func() { _ = late.Close() }()
		result := make(chan error, 1)
		go func() { result <- late.Invoke(t.Context(), unaryMethod, wrapperspb.String("late"), new(msg)) }()
		synctest.Wait() // dialed and sent, not accepted yet
		took := s.gracefulStop(t.Context())
		synctest.Wait()

		srv, other, served := serveEcho(t, transporttest.Mem)
		defer func() { srv.Stop(); <-served }()
		target.target.Store(&other)
		gated.admit <- struct{}{}
		if err := <-result; err != nil {
			t.Errorf("call on the late connection: %v", err)
		}
		if n := dialer.dials.Load(); n != 2 {
			t.Errorf("the late client dialed %d times, want 2", n)
		}
		close(release)
		if err := <-slow; err != nil {
			t.Errorf("slow call: %v", err)
		}
		if d := <-took; d != 0 {
			t.Errorf("GracefulStop took %v", d)
		}
	})
}

// TestGoAwayProtocolError: the server's unidirectional stream carries
// GOAWAY and nothing else. Anything else makes the client close the
// connection with PROTOCOL_ERROR; its calls fail with Unavailable.
func TestGoAwayProtocolError(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		l, d := f(t, 10)
		cc := rpc.NewClient(d)
		defer func() { _ = cc.Close() }()
		ctx := transporttest.Context(t)
		result := make(chan error, 1)
		go func() { result <- cc.Invoke(ctx, unaryMethod, wrapperspb.String("x"), new(msg)) }()
		conn, err := l.Accept(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.CloseWithError(transport.NoError, "") }()
		if _, err := conn.AcceptStream(ctx); err != nil {
			t.Fatal(err)
		}
		st, err := conn.OpenUniStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Write(frame.AppendFrame(nil, frame.TypeMessage, nil)); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()
		wantCode(t, <-result, codes.Unavailable, "call in flight")
		<-conn.Context().Done()
		ce, ok := errors.AsType[*transport.ConnError](context.Cause(conn.Context()))
		if !ok || ce.Code != transport.ProtocolError || !ce.Remote {
			t.Fatalf("connection closed with %v, want PROTOCOL_ERROR from the client", context.Cause(conn.Context()))
		}
	})
}

// TestGracefulStopRealTime repeats TestGracefulStop in real time, so that it
// also runs on QUIC.
func TestGracefulStopRealTime(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		started := make(chan struct{}, 1)
		s := newStoppable(t, f, &testServer{unary: func(_ context.Context, in *msg) (*msg, error) {
			started <- struct{}{}
			time.Sleep(200 * time.Millisecond)
			return in, nil
		}})
		ctx := transporttest.Context(t)
		result := s.call(ctx)
		<-started
		took := s.gracefulStop(ctx)
		if err := <-s.served; err != nil {
			t.Fatalf("Serve during GracefulStop: %v", err)
		}
		if err := <-result; err != nil {
			t.Errorf("call in flight: %v", err)
		}
		if d := <-took; d > 5*time.Second {
			t.Errorf("GracefulStop took %v", d)
		}
	})
}
