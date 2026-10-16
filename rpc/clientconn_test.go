package rpc_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// switchDialer lets a test point a ClientConn at another server.
type switchDialer struct {
	target atomic.Pointer[transport.Dialer]
}

func (s *switchDialer) Dial(ctx context.Context) (transport.Conn, error) {
	return (*s.target.Load()).Dial(ctx)
}

func TestRedialAfterConnectionLoss(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		start := func() (*rpc.Server, transport.Dialer, chan error) {
			l, d := f(t, 10)
			srv := rpc.NewServer()
			srv.RegisterService(&echoDesc, &testServer{})
			served := make(chan error, 1)
			go func() { served <- srv.Serve(l) }()
			return srv, d, served
		}
		first, d1, served1 := start()
		var dialer switchDialer
		dialer.target.Store(&d1)
		cc := rpc.NewClient(&dialer)
		defer func() { _ = cc.Close() }()

		ctx := transporttest.Context(t)
		if err := cc.Invoke(ctx, unaryMethod, wrapperspb.String("1"), new(msg)); err != nil {
			t.Fatalf("first call: %v", err)
		}
		first.Stop()
		<-served1

		second, d2, served2 := start()
		defer func() { second.Stop(); <-served2 }()
		dialer.target.Store(&d2)
		// The lost connection is noticed on the next call, which dials again.
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var err error
		for range 50 {
			if err = cc.Invoke(ctx, unaryMethod, wrapperspb.String("2"), new(msg)); err == nil {
				return
			}
			if status.Code(err) != codes.Unavailable {
				break
			}
		}
		t.Fatalf("calls after the server came back: %v", err)
	})
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
