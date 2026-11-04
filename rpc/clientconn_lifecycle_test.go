package rpc_test

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

type dialFunc func(context.Context) (transport.Conn, error)

func (f dialFunc) Dial(ctx context.Context) (transport.Conn, error) { return f(ctx) }

func invokeEcho(ctx context.Context, cc *rpc.ClientConn) error {
	return cc.Invoke(ctx, unaryMethod, wrapperspb.String("x"), new(msg))
}

func TestCloseDuringDial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, canceled := make(chan struct{}), make(chan struct{})
		var dials atomic.Int32
		cc := rpc.NewClient(dialFunc(func(ctx context.Context) (transport.Conn, error) {
			dials.Add(1)
			close(started)
			<-ctx.Done()
			close(canceled)
			return nil, ctx.Err()
		}))
		done := make(chan error, 9)
		go func() { done <- invokeEcho(t.Context(), cc) }()
		<-started
		for range 8 {
			go func() { done <- invokeEcho(t.Context(), cc) }()
		}
		synctest.Wait() // Every call is waiting for the same dial, without a deadline.
		if err := cc.Close(); err != nil {
			t.Fatal(err)
		}
		for range 9 {
			wantCode(t, <-done, codes.Canceled, "Close during Dial")
		}
		<-canceled
		wantCode(t, invokeEcho(t.Context(), cc), codes.Canceled, "call after Close")
		if dials.Load() != 1 {
			t.Fatalf("dialed %d times, want 1", dials.Load())
		}
	})
}

// Waiters must leave immediately even if a dialer takes time to acknowledge
// cancellation. A connection it returns late must be closed, not installed.
func TestCloseWakesDialWaitersAndClosesLateConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l, d := transporttest.Mem(t, 10)
		conn, err := d.Dial(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		peer, err := l.Accept(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		started, release := make(chan struct{}), make(chan struct{})
		cc := rpc.NewClient(dialFunc(func(context.Context) (transport.Conn, error) {
			close(started)
			<-release
			return conn, nil
		}))
		first, waiter := make(chan error, 1), make(chan error, 1)
		go func() { first <- invokeEcho(t.Context(), cc) }()
		<-started
		go func() {
			_, err := cc.NewStream(t.Context(), &echoDesc.Streams[0], bidiMethod)
			waiter <- err
		}()
		synctest.Wait()
		if err := cc.Close(); err != nil {
			t.Fatal(err)
		}
		wantCode(t, <-waiter, codes.Canceled, "stream waiting for Dial")
		close(release)
		wantCode(t, <-first, codes.Canceled, "late Dial result")
		if conn.Context().Err() == nil || peer.Context().Err() == nil {
			t.Fatal("late connection was not closed on both sides")
		}
	})
}

func TestDialWaiterCancellationIsIndependent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, d, served := serveEcho(t, transporttest.Mem)
		defer func() { srv.Stop(); <-served }()
		started, release := make(chan struct{}), make(chan struct{})
		var dialCtx context.Context
		var dials atomic.Int32
		cc := rpc.NewClient(dialFunc(func(ctx context.Context) (transport.Conn, error) {
			dials.Add(1)
			dialCtx = ctx
			close(started)
			select {
			case <-release:
				return d.Dial(ctx)
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}))
		defer func() { _ = cc.Close() }()
		first, waiter := make(chan error, 1), make(chan error, 1)
		go func() { first <- invokeEcho(t.Context(), cc) }()
		<-started
		ctx, cancel := context.WithCancel(t.Context())
		go func() { waiter <- invokeEcho(ctx, cc) }()
		synctest.Wait()
		cancel()
		wantCode(t, <-waiter, codes.Canceled, "canceled waiter")
		if dialCtx.Err() != nil {
			t.Fatal("canceling a waiter canceled another call's Dial")
		}
		close(release)
		if err := <-first; err != nil {
			t.Fatal(err)
		}
		if err := invokeEcho(t.Context(), cc); err != nil {
			t.Fatal(err)
		}
		if dials.Load() != 1 {
			t.Fatalf("dialed %d times, want 1", dials.Load())
		}
	})
}

func TestCanceledDialDoesNotCancelOtherCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv, d, served := serveEcho(t, transporttest.Mem)
		defer func() { srv.Stop(); <-served }()
		started := make(chan struct{})
		var dials atomic.Int32
		cc := rpc.NewClient(dialFunc(func(ctx context.Context) (transport.Conn, error) {
			if dials.Add(1) == 1 {
				close(started)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return d.Dial(ctx)
		}))
		defer func() { _ = cc.Close() }()
		ctx, cancel := context.WithCancel(t.Context())
		first, waiter := make(chan error, 1), make(chan error, 1)
		go func() { first <- invokeEcho(ctx, cc) }()
		<-started
		go func() { waiter <- invokeEcho(t.Context(), cc) }()
		synctest.Wait()
		cancel()
		wantCode(t, <-first, codes.Canceled, "canceled Dial")
		if err := <-waiter; err != nil {
			t.Fatalf("other call failed: %v", err)
		}
		if dials.Load() != 2 {
			t.Fatalf("dialed %d times, want 2", dials.Load())
		}
	})
}
