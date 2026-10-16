package rpc_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

func TestUnary(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{})
		ctx := transporttest.Context(t)
		for i := range 20 {
			in := fmt.Sprintf("ping %d", i)
			got, err := e.unary(ctx, in)
			if err != nil || got != in {
				t.Fatalf("call %d: got %q, %v; want %q, nil", i, got, err, in)
			}
		}
	})
}

func TestUnaryHandlerErrors(t *testing.T) {
	detailed, err := status.New(codes.FailedPrecondition, "chat is archived").WithDetails(wrapperspb.String("unarchive first"))
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	tests := []struct {
		name    string
		err     error
		code    codes.Code
		message string
	}{
		{"status with details", detailed.Err(), codes.FailedPrecondition, "chat is archived"},
		{"plain error", errors.New("boom"), codes.Unknown, "boom"},
		{"context error", context.Canceled, codes.Canceled, context.Canceled.Error()},
		{"wrapped status", fmt.Errorf("load: %w", status.Error(codes.NotFound, "no chat")), codes.NotFound,
			"load: rpc error: code = NotFound desc = no chat"},
		{"invalid UTF-8 message", status.Error(codes.Internal, "bad \xff byte"), codes.Internal, "bad � byte"},
	}
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				e := newEnv(t, f, &testServer{unary: func(context.Context, *msg) (*msg, error) { return nil, tt.err }})
				_, err := e.unary(transporttest.Context(t), "x")
				st := wantCode(t, err, tt.code, "call")
				if st.Message() != tt.message {
					t.Fatalf("message %q, want %q", st.Message(), tt.message)
				}
				if want := status.Convert(tt.err).Details(); len(want) > 0 {
					got := st.Details()
					if len(got) != len(want) || !proto.Equal(got[0].(proto.Message), want[0].(proto.Message)) {
						t.Fatalf("details %v, want %v", got, want)
					}
				}
			})
		}
	})
}

func TestUnimplemented(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{})
		ctx := transporttest.Context(t)
		for method, want := range map[string]string{
			"/test.v1.Echo/Missing":   "unknown method Missing for service test.v1.Echo",
			"/missing.v1.Service/Get": "unknown service missing.v1.Service",
		} {
			err := e.cc.Invoke(ctx, method, wrapperspb.String("x"), new(msg))
			if st := wantCode(t, err, codes.Unimplemented, method); st.Message() != want {
				t.Fatalf("%s: message %q, want %q", method, st.Message(), want)
			}
		}
	})
}

func TestNotAProtobufMessage(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{})
		err := e.cc.Invoke(transporttest.Context(t), unaryMethod, "not a message", new(msg))
		wantCode(t, err, codes.Internal, "Invoke with a string argument")
	})
}

func TestExpiredDeadline(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		var called atomic.Bool
		e := newEnv(t, f, &testServer{unary: func(_ context.Context, in *msg) (*msg, error) {
			called.Store(true)
			return in, nil
		}})
		ctx, cancel := context.WithDeadline(transporttest.Context(t), time.Now().Add(-time.Second))
		defer cancel()
		_, err := e.unary(ctx, "x")
		wantCode(t, err, codes.DeadlineExceeded, "call with an expired deadline")
		if called.Load() {
			t.Fatal("the handler ran for a call whose deadline had passed")
		}
	})
}
