package rpc_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// stoppable is a server and a client for the tests of GracefulStop.
type stoppable struct {
	srv    *rpc.Server
	l      transport.Listener
	d      transport.Dialer
	cc     *rpc.ClientConn
	served chan error
}

func newStoppable(t *testing.T, f transporttest.Factory, impl *testServer) *stoppable {
	t.Helper()
	l, d := f(t, 10)
	s := &stoppable{srv: rpc.NewServer(), l: l, d: d, cc: rpc.NewClient(d), served: make(chan error, 1)}
	s.srv.RegisterService(&echoDesc, impl)
	go func() { s.served <- s.srv.Serve(l) }()
	t.Cleanup(func() { _ = s.cc.Close(); s.srv.Stop() })
	return s
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

// TestGracefulStop: the listener closes at once and Serve returns nil; the
// call in flight and a call made meanwhile on the open connection finish;
// GracefulStop returns at the first moment without calls in flight.
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
		// Due at 500 ms, done at 1.5 s; the first call is done at 1.1 s.
		time.Sleep(400 * time.Millisecond)
		second := s.call(t.Context())
		for _, r := range []<-chan error{first, second} {
			if err := <-r; err != nil {
				t.Errorf("call: %v", err)
			}
		}
		if d := <-took; d != 1400*time.Millisecond {
			t.Errorf("GracefulStop took %v, want 1.4 s", d)
		}
		if err := s.srv.Serve(s.l); !errors.Is(err, rpc.ErrServerStopped) {
			t.Errorf("Serve after GracefulStop: %v", err)
		}
	})
}

// TestGracefulStopIdle: without calls in flight GracefulStop stops at once.
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

// TestGracefulStopRealTime repeats TestGracefulStop in real time, so that it
// also runs on QUIC.
func TestGracefulStopRealTime(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		s := newStoppable(t, f, sleepingHandler(200*time.Millisecond))
		ctx := transporttest.Context(t)
		// Warm the connection up, so that the call below is in flight at once.
		if err := <-s.call(ctx); err != nil {
			t.Fatal(err)
		}
		result := s.call(ctx)
		time.Sleep(50 * time.Millisecond)
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
