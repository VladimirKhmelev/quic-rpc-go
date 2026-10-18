package difftestv1_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"

	difftestv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/difftest/v1"
)

// The smoke test shows that the generated grpc-go stubs compile and carry
// unary and bidi calls; the differential tests compare behavior.

type server struct {
	difftestv1.UnimplementedDiffTestServer
}

func (server) Unary(_ context.Context, in *difftestv1.Request) (*difftestv1.Response, error) {
	return &difftestv1.Response{Seq: in.GetSeq(), Payload: in.GetPayload()}, nil
}

func (server) Bidi(stream difftestv1.DiffTest_BidiServer) error {
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
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	difftestv1.RegisterDiffTestServer(srv, server{})
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufconn",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc.NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	client := difftestv1.NewDiffTestClient(conn)
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
