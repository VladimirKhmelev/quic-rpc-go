package rpc_test

import (
	"errors"
	"io"
	"slices"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

// The tests below drive server- and client-streaming calls through the
// typed streams, as generated code does (RPC-2).

// startServerStream starts a server-streaming call with the request in.
func startServerStream(t *testing.T, e *env, in string) rpc.ServerStreamingClient[msg] {
	t.Helper()
	stream, err := e.cc.NewStream(transporttest.Context(t), &echoDesc.Streams[1], serverStreamMethod)
	if err != nil {
		t.Fatal(err)
	}
	x := &rpc.GenericClientStream[msg, msg]{ClientStream: stream}
	if err := x.SendMsg(wrapperspb.String(in)); err != nil {
		t.Fatal(err)
	}
	if err := x.CloseSend(); err != nil {
		t.Fatal(err)
	}
	return x
}

// startClientStream starts a client-streaming call.
func startClientStream(t *testing.T, e *env) rpc.ClientStreamingClient[msg, msg] {
	t.Helper()
	stream, err := e.cc.NewStream(transporttest.Context(t), &echoDesc.Streams[2], clientStreamMethod)
	if err != nil {
		t.Fatal(err)
	}
	return &rpc.GenericClientStream[msg, msg]{ClientStream: stream}
}

// recvAll receives up to the end of the call.
func recvAll(stream rpc.ServerStreamingClient[msg]) ([]string, error) {
	var got []string
	for {
		m, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return got, nil
		}
		if err != nil {
			return got, err
		}
		got = append(got, m.GetValue())
	}
}

func TestServerStreaming(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{})
		got, err := recvAll(startServerStream(t, e, "abc"))
		if err != nil || !slices.Equal(got, []string{"a", "b", "c"}) {
			t.Fatalf("got %q, %v", got, err)
		}
	})
}

// TestServerStreamingError: the responses sent before the error arrive,
// then the status.
func TestServerStreamingError(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{serverStream: func(in *msg, stream serverStreamServer) error {
			for range 2 {
				if err := stream.Send(in); err != nil {
					return err
				}
			}
			return status.Error(codes.Aborted, "enough")
		}})
		got, err := recvAll(startServerStream(t, e, "x"))
		if !slices.Equal(got, []string{"x", "x"}) {
			t.Errorf("got %q before the status", got)
		}
		wantCode(t, err, codes.Aborted, "server stream")
	})
}

func TestClientStreaming(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{})
		stream := startClientStream(t, e)
		for _, v := range []string{"a", "b", "c"} {
			if err := stream.Send(wrapperspb.String(v)); err != nil {
				t.Fatal(err)
			}
		}
		resp, err := stream.CloseAndRecv()
		if err != nil || resp.GetValue() != "abc" {
			t.Fatalf("CloseAndRecv: %q, %v", resp.GetValue(), err)
		}
	})
}

// TestClientStreamingEarlyAnswer: the server answers after the first
// request and does not read the rest. The client's later sends go nowhere,
// and CloseAndRecv returns the answer.
func TestClientStreamingEarlyAnswer(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		answered := make(chan struct{})
		e := newEnv(t, f, &testServer{clientStream: func(stream clientStreamServer) error {
			defer close(answered)
			in, err := stream.Recv()
			if err != nil {
				return err
			}
			return stream.SendAndClose(in)
		}})
		stream := startClientStream(t, e)
		if err := stream.Send(wrapperspb.String("first")); err != nil {
			t.Fatal(err)
		}
		<-answered
		if err := stream.Send(wrapperspb.String("second")); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("Send after the answer: %v", err)
		}
		resp, err := stream.CloseAndRecv()
		if err != nil || resp.GetValue() != "first" {
			t.Fatalf("CloseAndRecv: %q, %v", resp.GetValue(), err)
		}
	})
}

// TestClientStreamingError: a handler that fails without an answer ends the
// call with its status.
func TestClientStreamingError(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{clientStream: func(stream clientStreamServer) error {
			for {
				if _, err := stream.Recv(); errors.Is(err, io.EOF) {
					return status.Error(codes.InvalidArgument, "no answer")
				} else if err != nil {
					return err
				}
			}
		}})
		stream := startClientStream(t, e)
		if err := stream.Send(wrapperspb.String("a")); err != nil {
			t.Fatal(err)
		}
		resp, err := stream.CloseAndRecv()
		wantCode(t, err, codes.InvalidArgument, "client stream")
		if resp != nil {
			t.Errorf("CloseAndRecv returned %v with the error", resp)
		}
	})
}
