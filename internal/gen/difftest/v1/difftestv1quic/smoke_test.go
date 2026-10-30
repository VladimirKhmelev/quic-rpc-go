package difftestv1quic_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	difftestv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/difftest/v1"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/gen/difftest/v1/difftestv1quic"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/mem"
)

// The smoke test shows that the generated quic-rpc-go stubs compile and
// carry unary and bidi calls; the differential tests compare behavior.

type server struct {
	difftestv1quic.UnimplementedDiffTestServer
}

func (server) Unary(_ context.Context, in *difftestv1.Request) (*difftestv1.Response, error) {
	return &difftestv1.Response{Seq: in.GetSeq(), Payload: in.GetPayload()}, nil
}

func (server) Bidi(stream difftestv1quic.DiffTest_BidiServer) error {
	for {
		in, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&difftestv1.Response{Seq: in.GetSeq()}); err != nil {
			return err
		}
	}
}

func TestSmoke(t *testing.T) {
	l := mem.Listen(mem.Options{})
	srv := rpc.NewServer()
	difftestv1quic.RegisterDiffTestServer(srv, server{})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	cc := rpc.NewClient(l)
	t.Cleanup(func() { _ = cc.Close() })
	client := difftestv1quic.NewDiffTestClient(cc)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	resp, err := client.Unary(ctx, &difftestv1.Request{Seq: 7, Payload: []byte("ping")})
	if err != nil || resp.GetSeq() != 7 || string(resp.GetPayload()) != "ping" {
		t.Fatalf("Unary: %v, %v", resp, err)
	}

	stream, err := client.Bidi(ctx)
	if err != nil {
		t.Fatalf("Bidi: %v", err)
	}
	for seq := range int32(3) {
		if err := stream.Send(&difftestv1.Request{Seq: seq}); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if got, err := stream.Recv(); err != nil || got.GetSeq() != seq {
			t.Fatalf("Recv: %v, %v; want seq %d", got, err, seq)
		}
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("final Recv: %v, want io.EOF", err)
	}
}

// TestSmokeInterceptors: the generated handlers run the server's unary
// interceptors with the request decoded and the method's full name, and the
// generated client goes through the client's.
func TestSmokeInterceptors(t *testing.T) {
	var onServer, onClient []string
	l := mem.Listen(mem.Options{})
	srv := rpc.NewServer(rpc.ChainUnaryInterceptor(func(ctx context.Context, req any, info *rpc.UnaryServerInfo, handler rpc.UnaryHandler) (any, error) {
		onServer = append(onServer, info.FullMethod)
		if _, ok := info.Server.(server); !ok {
			t.Errorf("info.Server is %T", info.Server)
		}
		req.(*difftestv1.Request).Seq++
		return handler(ctx, req)
	}))
	difftestv1quic.RegisterDiffTestServer(srv, server{})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	cc := rpc.NewClient(l, rpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *rpc.ClientConn, invoker rpc.UnaryInvoker, opts ...rpc.CallOption) error {
		onClient = append(onClient, method)
		return invoker(ctx, method, req, reply, cc, opts...)
	}))
	t.Cleanup(func() { _ = cc.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	resp, err := difftestv1quic.NewDiffTestClient(cc).Unary(ctx, &difftestv1.Request{Seq: 7})
	if err != nil || resp.GetSeq() != 8 {
		t.Fatalf("Unary: %v, %v; want seq 8", resp, err)
	}
	want := []string{difftestv1quic.DiffTest_Unary_FullMethodName}
	if !slices.Equal(onServer, want) || !slices.Equal(onClient, want) {
		t.Errorf("interceptors saw %q on the server, %q on the client", onServer, onClient)
	}
}
