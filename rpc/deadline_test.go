package rpc_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

// These tests run on the mem transport inside a synctest bubble: time is
// fake, so deadlines are exact and nothing sleeps, and the bubble fails the
// test if any goroutine is still alive once the server and the client are
// shut down. That is the goroutine-leak check for cancellation (RPC-6).

func inBubble(t *testing.T, impl *testServer, fn func(t *testing.T, e *env)) {
	synctest.Test(t, func(t *testing.T) {
		fn(t, newEnv(t, transporttest.Mem, impl))
	})
}

// TestDeadlinePropagation checks RPC-5: the server's deadline is exactly the
// time the client had left. Only durations cross the wire, so the clocks of
// the two sides need not agree.
func TestDeadlinePropagation(t *testing.T) {
	left := make(chan time.Duration, 1)
	impl := &testServer{unary: func(ctx context.Context, in *msg) (*msg, error) {
		if deadline, ok := ctx.Deadline(); ok {
			left <- time.Until(deadline)
		} else {
			left <- -1
		}
		return in, nil
	}}
	inBubble(t, impl, func(t *testing.T, e *env) {
		for _, tt := range []struct {
			timeout time.Duration
			want    time.Duration
		}{
			{5 * time.Second, 5 * time.Second},
			{1500 * time.Nanosecond, 2 * time.Microsecond}, // rounded up to whole microseconds
			{0, -1}, // no deadline
		} {
			ctx := t.Context()
			if tt.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tt.timeout)
				defer cancel()
			}
			if _, err := e.unary(ctx, "x"); err != nil {
				t.Fatalf("call with timeout %v: %v", tt.timeout, err)
			}
			if got := <-left; got != tt.want {
				t.Errorf("timeout %v: the server had %v left, want %v", tt.timeout, got, tt.want)
			}
		}
	})
}

func TestDeadlineExceeded(t *testing.T) {
	handlerErr := make(chan error, 1)
	impl := &testServer{unary: func(ctx context.Context, _ *msg) (*msg, error) {
		<-ctx.Done()
		handlerErr <- ctx.Err()
		return nil, ctx.Err()
	}}
	inBubble(t, impl, func(t *testing.T, e *env) {
		start := time.Now()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err := e.unary(ctx, "x")
		wantCode(t, err, codes.DeadlineExceeded, "call")
		if elapsed := time.Since(start); elapsed != time.Second {
			t.Errorf("the call failed after %v, want exactly 1s", elapsed)
		}
		if err := <-handlerErr; err == nil {
			t.Error("the handler's context was not canceled")
		}
	})
}

// TestCancellationStopsHandler checks RPC-6 for a unary call.
func TestCancellationStopsHandler(t *testing.T) {
	started := make(chan struct{})
	handlerErr := make(chan error, 1)
	impl := &testServer{unary: func(ctx context.Context, _ *msg) (*msg, error) {
		close(started)
		<-ctx.Done()
		handlerErr <- ctx.Err()
		return nil, ctx.Err()
	}}
	inBubble(t, impl, func(t *testing.T, e *env) {
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() {
			_, err := e.unary(ctx, "x")
			result <- err
		}()
		<-started
		cancel()
		wantCode(t, <-result, codes.Canceled, "canceled call")
		if err := <-handlerErr; !errors.Is(err, context.Canceled) {
			t.Fatalf("handler context error %v, want context.Canceled", err)
		}
	})
}

// TestCancellationUnblocksServerRecv checks RPC-6 for a handler blocked in Recv.
func TestCancellationUnblocksServerRecv(t *testing.T) {
	recvErr := make(chan error, 1)
	impl := &testServer{bidi: func(stream bidiServer) error {
		_, err := stream.Recv()
		recvErr <- err
		return err
	}}
	inBubble(t, impl, func(t *testing.T, e *env) {
		ctx, cancel := context.WithCancel(t.Context())
		s := mustBidi(t)(e.bidi(ctx))
		synctest.Wait() // the handler is blocked in Recv
		cancel()
		if _, err := s.Recv(); status.Code(err) != codes.Canceled {
			t.Fatalf("client Recv after cancel: %v, want Canceled", err)
		}
		if err := <-recvErr; status.Code(err) != codes.Canceled {
			t.Fatalf("server Recv after the client canceled: %v, want Canceled", err)
		}
	})
}

// TestCancellationUnblocksServerSend checks that a handler blocked in Send by
// flow control is released when the client cancels.
func TestCancellationUnblocksServerSend(t *testing.T) {
	sendErr := make(chan error, 1)
	chunk := wrapperspb.String(strings.Repeat("x", 100<<10))
	impl := &testServer{bidi: func(stream bidiServer) error {
		for {
			if err := stream.Send(chunk); err != nil {
				sendErr <- err
				return err
			}
		}
	}}
	inBubble(t, impl, func(t *testing.T, e *env) {
		ctx, cancel := context.WithCancel(t.Context())
		mustBidi(t)(e.bidi(ctx)) // never reads
		synctest.Wait()          // the handler is blocked in Send: the window is full
		cancel()
		if err := <-sendErr; status.Code(err) != codes.Canceled {
			t.Fatalf("server Send after the client canceled: %v, want Canceled", err)
		}
	})
}

func TestStopCancelsHandlers(t *testing.T) {
	started := make(chan struct{})
	impl := &testServer{unary: func(ctx context.Context, _ *msg) (*msg, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	synctest.Test(t, func(t *testing.T) {
		l, d := transporttest.Mem(t, 10)
		srv := rpc.NewServer()
		srv.RegisterService(&echoDesc, impl)
		served := make(chan error, 1)
		go func() { served <- srv.Serve(l) }()
		e := &env{cc: rpc.NewClient(d)}
		defer func() { _ = e.cc.Close() }()

		result := make(chan error, 1)
		go func() {
			_, err := e.unary(t.Context(), "x")
			result <- err
		}()
		<-started
		srv.Stop()
		wantCode(t, <-result, codes.Unavailable, "call in flight during Stop")
		if err := <-served; err != nil {
			t.Fatalf("Serve: %v", err)
		}
	})
}

func TestManyCallsLeaveNothingBehind(t *testing.T) {
	inBubble(t, &testServer{}, func(t *testing.T, e *env) {
		for range 50 {
			if _, err := e.unary(t.Context(), "x"); err != nil {
				t.Fatalf("unary: %v", err)
			}
		}
		for range 10 {
			s := mustBidi(t)(e.bidi(t.Context()))
			if err := s.Send(wrapperspb.String("x")); err != nil {
				t.Fatalf("Send: %v", err)
			}
			if err := s.CloseSend(); err != nil {
				t.Fatalf("CloseSend: %v", err)
			}
			for {
				if _, err := s.Recv(); err != nil {
					break
				}
			}
		}
	})
}

// TestDeadlineAndCancelRealTime repeats the deadline and cancellation
// scenarios above with real time, so they also run on QUIC.
func TestDeadlineAndCancelRealTime(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		started := make(chan struct{}, 1)
		handlerDone := make(chan error, 1)
		e := newEnv(t, f, &testServer{unary: func(ctx context.Context, _ *msg) (*msg, error) {
			started <- struct{}{}
			<-ctx.Done()
			handlerDone <- ctx.Err()
			return nil, ctx.Err()
		}})

		ctx, cancel := context.WithTimeout(transporttest.Context(t), 100*time.Millisecond)
		defer cancel()
		_, err := e.unary(ctx, "deadline")
		wantCode(t, err, codes.DeadlineExceeded, "call with a 100ms deadline")
		<-started
		if err := <-handlerDone; err == nil {
			t.Fatal("the handler's context was not canceled after the deadline")
		}

		ctx, cancel = context.WithCancel(transporttest.Context(t))
		result := make(chan error, 1)
		go func() {
			_, err := e.unary(ctx, "cancel")
			result <- err
		}()
		<-started
		cancel()
		wantCode(t, <-result, codes.Canceled, "canceled call")
		if err := <-handlerDone; !errors.Is(err, context.Canceled) {
			t.Fatalf("handler context error %v, want context.Canceled", err)
		}
	})
}
