package h2pool_test

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/baseline/h2"
	"github.com/VladimirKhmelev/quic-rpc-go/bench/baseline/h2pool"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/certs"
	benchv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1"
)

type server struct {
	benchv1.UnimplementedBenchServer
	connected    chan struct{}
	disconnected chan struct{}
	waiting      chan struct{}
	canceled     chan struct{}
}

func (*server) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context { return ctx }
func (*server) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context   { return ctx }
func (*server) HandleRPC(context.Context, stats.RPCStats)                         {}
func (s *server) HandleConn(_ context.Context, event stats.ConnStats) {
	switch event.(type) {
	case *stats.ConnBegin:
		s.connected <- struct{}{}
	case *stats.ConnEnd:
		s.disconnected <- struct{}{}
	}
}

func (s *server) Call(ctx context.Context, req *benchv1.CallRequest) (*benchv1.CallResponse, error) {
	if string(req.Payload) == "wait" {
		s.waiting <- struct{}{}
		<-ctx.Done()
		s.canceled <- struct{}{}
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	p, _ := peer.FromContext(ctx)
	return &benchv1.CallResponse{Payload: []byte(p.Addr.String())}, nil
}

func (*server) Echo(stream grpc.BidiStreamingServer[benchv1.EchoMessage, benchv1.EchoMessage]) error {
	p, _ := peer.FromContext(stream.Context())
	for {
		in, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(&benchv1.EchoMessage{Seq: in.Seq, Payload: []byte(p.Addr.String())}); err != nil {
			return err
		}
	}
}

func fixture(t *testing.T, n int, opts ...grpc.DialOption) (*h2pool.Pool, *server) {
	t.Helper()
	pair, err := certs.New("localhost")
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{connected: make(chan struct{}, 128), disconnected: make(chan struct{}, 128), waiting: make(chan struct{}, 1), canceled: make(chan struct{}, 1)}
	srv := h2.NewServer(pair.Server, grpc.StatsHandler(s))
	benchv1.RegisterBenchServer(srv, s)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	p, err := h2pool.Dial(l.Addr().String(), pair.Client, n, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p, s
}

func wait(ctx context.Context, t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Server-observed peer addresses prove that the channels use distinct TCP
// connections and that RPCs rotate across them, including a live bidi stream.
func TestConnectionsAndRoundRobin(t *testing.T) {
	for _, n := range []int{1, 2, 4, 8} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			ctx := testContext(t)
			p, srv := fixture(t, n)
			if err := p.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			for range n {
				wait(ctx, t, srv.connected)
			}
			client := benchv1.NewBenchClient(p)
			var addresses []string
			seen := map[string]bool{}
			for i := range 2 * n {
				resp, err := client.Call(ctx, &benchv1.CallRequest{})
				if err != nil {
					t.Fatal(err)
				}
				addr := string(resp.Payload)
				if i < n {
					addresses = append(addresses, addr)
					seen[addr] = true
				} else if addr != addresses[i%n] {
					t.Fatal("RPC did not rotate to the next connection")
				}
			}
			if len(seen) != n {
				t.Fatalf("%d physical connections, want %d", len(seen), n)
			}
			stream, err := client.Echo(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for i := range n + 1 {
				if err := stream.Send(&benchv1.EchoMessage{Seq: uint64(i)}); err != nil {
					t.Fatal(err)
				}
				resp, err := stream.Recv()
				if err != nil || string(resp.GetPayload()) != addresses[0] || resp.GetSeq() != uint64(i) {
					t.Fatalf("bidi changed connection: %v, %v", resp, err)
				}
				respCall, err := client.Call(ctx, &benchv1.CallRequest{})
				if err != nil || string(respCall.GetPayload()) != addresses[(i+1)%n] {
					t.Fatalf("RPC rotation while bidi active: %v", err)
				}
			}
			_ = stream.CloseSend()
			if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			for range n {
				wait(ctx, t, srv.disconnected)
			}
		})
	}
}

// Every dial must have started before any is allowed to finish. This also
// checks cancellation of a Connect wait without destroying the pool.
func TestParallelConnectAndCancellation(t *testing.T) {
	ctx := testContext(t)
	const n = 4
	started, release := make(chan struct{}, n), make(chan struct{})
	var releaseOnce sync.Once
	finish := func() { releaseOnce.Do(func() { close(release) }) }
	defer finish()
	p, srv := fixture(t, n, grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}))
	waitCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- p.Connect(waitCtx) }()
	for range n {
		wait(ctx, t, started)
	}
	cancel()
	select {
	case err := <-done:
		if status.Code(err) != codes.Canceled {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	finish()
	if err := p.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	for range n {
		wait(ctx, t, srv.connected)
	}
}

func TestCloseCancelsRPC(t *testing.T) {
	ctx := testContext(t)
	p, srv := fixture(t, 4)
	if err := p.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := benchv1.NewBenchClient(p).Call(ctx, &benchv1.CallRequest{Payload: []byte("wait")})
		done <- err
	}()
	wait(ctx, t, srv.waiting)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if status.Code(err) != codes.Canceled {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	wait(ctx, t, srv.canceled)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidSize(t *testing.T) {
	for _, n := range []int{-1, 0} {
		if p, err := h2pool.Dial("localhost:1", nil, n); err == nil {
			_ = p.Close()
			t.Fatal("accepted invalid size")
		}
	}
}
