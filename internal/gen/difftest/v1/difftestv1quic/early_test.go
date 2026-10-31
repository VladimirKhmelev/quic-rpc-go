package difftestv1quic_test

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	difftestv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/difftest/v1"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/gen/difftest/v1/difftestv1quic"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/mem"
)

type recordingDialer struct {
	*mem.Listener
	early    atomic.Int32
	ordinary atomic.Int32
}

func (d *recordingDialer) Dial(ctx context.Context) (transport.Conn, error) {
	c, err := d.Listener.Dial(ctx)
	if err != nil {
		return nil, err
	}
	return &recordingConn{Conn: c, d: d}, nil
}

type recordingConn struct {
	transport.Conn
	d *recordingDialer
}

func (c *recordingConn) OpenStreamEarly(ctx context.Context) (transport.Stream, error) {
	c.d.early.Add(1)
	return c.Conn.OpenStream(ctx)
}

func (c *recordingConn) OpenStream(ctx context.Context) (transport.Stream, error) {
	c.d.ordinary.Add(1)
	return c.Conn.OpenStream(ctx)
}

func (*recordingConn) WaitHandshake(context.Context) error { return nil }

// Generated annotations must reach the actual RPC transport for every shape,
// and an explicit call option must override the generated default.
func TestGeneratedIdempotency(t *testing.T) {
	d := &recordingDialer{Listener: mem.Listen(mem.Options{})}
	srv := rpc.NewServer()
	difftestv1quic.RegisterDiffTestServer(srv, server{})
	go func() { _ = srv.Serve(d.Listener) }()
	t.Cleanup(srv.Stop)
	cc := rpc.NewClient(d)
	t.Cleanup(func() { _ = cc.Close() })
	client := difftestv1quic.NewDiffTestClient(cc)
	ctx := t.Context()
	for _, opts := range [][]rpc.CallOption{nil, {rpc.Idempotent(false)}} {
		_, _ = client.Unary(ctx, &difftestv1.Request{}, opts...)
		bidi, err := client.Bidi(ctx, opts...)
		if err != nil {
			t.Fatal(err)
		}
		_ = bidi.CloseSend()
		if _, err := bidi.Recv(); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		ss, err := client.ServerStream(ctx, &difftestv1.Request{}, opts...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ss.Recv(); status.Code(err) != codes.Unimplemented {
			t.Fatalf("ServerStream: %v", err)
		}
		cs, err := client.ClientStream(ctx, opts...)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cs.CloseAndRecv(); status.Code(err) != codes.Unimplemented {
			t.Fatalf("ClientStream: %v", err)
		}
	}
	if d.early.Load() != 4 || d.ordinary.Load() != 4 {
		t.Fatalf("opens: early=%d ordinary=%d", d.early.Load(), d.ordinary.Load())
	}
	desc := &difftestv1quic.DiffTest_ServiceDesc
	if !desc.Methods[0].Idempotent {
		t.Fatal("unary server annotation missing")
	}
	for _, stream := range desc.Streams {
		if !stream.Idempotent {
			t.Fatalf("%s server annotation missing", stream.StreamName)
		}
	}
}
