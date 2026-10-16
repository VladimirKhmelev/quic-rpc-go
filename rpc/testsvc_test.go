package rpc_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

// The test service is described by hand the way generated code describes
// services: an interface, handlers and a ServiceDesc.

type (
	msg        = wrapperspb.StringValue
	bidiServer = rpc.BidiStreamingServer[msg, msg]
)

type echoService interface {
	Unary(context.Context, *msg) (*msg, error)
	Bidi(bidiServer) error
}

const unaryMethod = "/test.v1.Echo/Unary"

var echoDesc = rpc.ServiceDesc{
	ServiceName: "test.v1.Echo",
	HandlerType: (*echoService)(nil),
	Methods:     []rpc.MethodDesc{{MethodName: "Unary", Handler: unaryHandler}},
	Streams: []rpc.StreamDesc{{
		StreamName:    "Bidi",
		Handler:       bidiHandler,
		ServerStreams: true,
		ClientStreams: true,
	}},
	Metadata: "test.proto",
}

func unaryHandler(srv any, ctx context.Context, dec func(any) error, _ rpc.UnaryServerInterceptor) (any, error) { //nolint:revive // rpc.MethodHandler mirrors grpc-go, ctx comes second
	in := new(msg)
	if err := dec(in); err != nil {
		return nil, err
	}
	return srv.(echoService).Unary(ctx, in)
}

func bidiHandler(srv any, stream rpc.ServerStream) error {
	return srv.(echoService).Bidi(&rpc.GenericServerStream[msg, msg]{ServerStream: stream})
}

// testServer echoes by default; tests replace the behavior they need.
type testServer struct {
	unary func(context.Context, *msg) (*msg, error)
	bidi  func(bidiServer) error
}

func (s *testServer) Unary(ctx context.Context, in *msg) (*msg, error) {
	if s.unary == nil {
		return in, nil
	}
	return s.unary(ctx, in)
}

func (s *testServer) Bidi(stream bidiServer) error {
	if s.bidi == nil {
		return echoAll(stream)
	}
	return s.bidi(stream)
}

func echoAll(stream bidiServer) error {
	for {
		in, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(in); err != nil {
			return err
		}
	}
}

type env struct {
	cc *rpc.ClientConn
}

// newEnv serves impl on a fresh transport and returns a client for it.
// Everything is stopped when the test ends.
func newEnv(t *testing.T, newTransport transporttest.Factory, impl *testServer) *env {
	t.Helper()
	return newEnvWithLimit(t, newTransport, impl, 100)
}

func newEnvWithLimit(t *testing.T, newTransport transporttest.Factory, impl *testServer, maxStreams int) *env {
	t.Helper()
	l, d := newTransport(t, maxStreams)
	srv := rpc.NewServer()
	srv.RegisterService(&echoDesc, impl)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	cc := rpc.NewClient(d)
	t.Cleanup(func() {
		_ = cc.Close()
		srv.Stop()
		// A short test may stop the server before the goroutine reaches
		// Serve, which then reports ErrServerStopped.
		if err := <-served; err != nil && !errors.Is(err, rpc.ErrServerStopped) {
			t.Errorf("Serve: %v", err)
		}
	})
	return &env{cc: cc}
}

func (e *env) unary(ctx context.Context, in string, opts ...rpc.CallOption) (string, error) {
	out := new(msg)
	err := e.cc.Invoke(ctx, unaryMethod, wrapperspb.String(in), out, opts...)
	return out.GetValue(), err
}

func forEachTransport(t *testing.T, fn func(t *testing.T, newTransport transporttest.Factory)) {
	for _, tr := range transporttest.Transports {
		t.Run(tr.Name, func(t *testing.T) { fn(t, tr.Factory) })
	}
}

func wantCode(t *testing.T, err error, want codes.Code, what string) *status.Status {
	t.Helper()
	st := status.Convert(err)
	if st.Code() != want {
		t.Fatalf("%s: got %v, want code %v", what, err, want)
	}
	return st
}
