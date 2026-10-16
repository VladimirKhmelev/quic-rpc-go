package rpc_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

func mustPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", what)
		}
	}()
	fn()
}

func TestRegisterServicePanics(t *testing.T) {
	mustPanic(t, "registering an implementation of the wrong type", func() {
		rpc.NewServer().RegisterService(&echoDesc, struct{}{})
	})
	mustPanic(t, "registering a service twice", func() {
		srv := rpc.NewServer()
		srv.RegisterService(&echoDesc, &testServer{})
		srv.RegisterService(&echoDesc, &testServer{})
	})
	mustPanic(t, "registering after Serve", func() {
		l, d := transporttest.Mem(t, 10)
		srv := rpc.NewServer()
		served := make(chan error, 1)
		go func() { served <- srv.Serve(l) }()
		cc := rpc.NewClient(d)
		defer func() {
			_ = cc.Close()
			srv.Stop()
			<-served
		}()
		// An answer proves Serve is running.
		err := cc.Invoke(transporttest.Context(t), unaryMethod, &msg{}, new(msg))
		if status.Code(err) != codes.Unimplemented {
			t.Fatalf("call to a server without services: %v", err)
		}
		srv.RegisterService(&echoDesc, &testServer{})
	})
}

func TestServeAndStop(t *testing.T) {
	srv := rpc.NewServer()
	l, _ := transporttest.Mem(t, 10)
	srv.Stop()
	if err := srv.Serve(l); !errors.Is(err, rpc.ErrServerStopped) {
		t.Fatalf("Serve after Stop: %v, want ErrServerStopped", err)
	}
	srv.Stop() // a second Stop is a no-op

	closed, _ := transporttest.Mem(t, 10)
	if err := closed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := rpc.NewServer().Serve(closed); !errors.Is(err, transport.ErrListenerClosed) {
		t.Fatalf("Serve on a closed listener: %v, want ErrListenerClosed", err)
	}
}

func TestServerStopFailsCallsInFlight(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		started := make(chan struct{})
		l, d := f(t, 10)
		srv := rpc.NewServer()
		srv.RegisterService(&echoDesc, &testServer{unary: func(ctx context.Context, _ *msg) (*msg, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}})
		served := make(chan error, 1)
		go func() { served <- srv.Serve(l) }()
		cc := rpc.NewClient(d)
		defer func() { _ = cc.Close() }()

		result := make(chan error, 1)
		go func() {
			result <- cc.Invoke(transporttest.Context(t), unaryMethod, wrapperspb.String("x"), new(msg))
		}()
		<-started
		srv.Stop()
		wantCode(t, <-result, codes.Unavailable, "call in flight during Stop")
		if err := <-served; err != nil {
			t.Fatalf("Serve after Stop: %v", err)
		}
	})
}
