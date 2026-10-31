package rpc_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/mem"
)

// handshakeConn makes the authentication boundary controllable with synctest.
// QUIC acceptance and rejection are tested separately on actual UDP sockets.
type handshakeConn struct {
	transport.Conn
	ready <-chan struct{}
	opens *atomic.Int32
}

func (c *handshakeConn) WaitHandshake(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.Context().Done():
		return context.Cause(c.Context())
	case <-c.ready:
		return nil
	}
}

func (c *handshakeConn) OpenStream(ctx context.Context) (transport.Stream, error) {
	if err := c.WaitHandshake(ctx); err != nil {
		return nil, err
	}
	return c.OpenStreamEarly(ctx)
}

func (c *handshakeConn) OpenStreamEarly(ctx context.Context) (transport.Stream, error) {
	c.opens.Add(1)
	return c.Conn.OpenStream(ctx)
}

type handshakeTransport struct {
	*mem.Listener
	ready chan struct{}
	opens atomic.Int32
}

func (d *handshakeTransport) Dial(ctx context.Context) (transport.Conn, error) {
	c, err := d.Listener.Dial(ctx)
	if err != nil {
		return nil, err
	}
	return &handshakeConn{Conn: c, ready: d.ready, opens: &d.opens}, nil
}

func (d *handshakeTransport) Accept(ctx context.Context) (transport.Conn, error) {
	c, err := d.Listener.Accept(ctx)
	if err != nil {
		return nil, err
	}
	return &handshakeConn{Conn: c, ready: d.ready, opens: &d.opens}, nil
}

func handshakeEnv(t *testing.T, marked bool, onHandler func(context.Context)) (*rpc.ClientConn, *handshakeTransport) {
	t.Helper()
	d := &handshakeTransport{Listener: mem.Listen(mem.Options{}), ready: make(chan struct{})}
	desc := echoDesc
	desc.Methods = append([]rpc.MethodDesc(nil), desc.Methods...)
	desc.Streams = append([]rpc.StreamDesc(nil), desc.Streams...)
	for i := range desc.Methods {
		desc.Methods[i].Idempotent = marked
	}
	for i := range desc.Streams {
		desc.Streams[i].Idempotent = marked
	}
	srv := rpc.NewServer(
		rpc.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *rpc.UnaryServerInfo, h rpc.UnaryHandler) (any, error) {
			onHandler(ctx)
			return h(ctx, req)
		}),
		rpc.ChainStreamInterceptor(func(srv any, ss rpc.ServerStream, _ *rpc.StreamServerInfo, h rpc.StreamHandler) error {
			onHandler(ss.Context())
			return h(srv, ss)
		}),
	)
	srv.RegisterService(&desc, &testServer{})
	served := make(chan error, 1)
	go func() { served <- srv.Serve(d) }()
	cc := rpc.NewClient(d)
	t.Cleanup(func() { _ = cc.Close(); srv.Stop(); <-served })
	return cc, d
}

func gatedCall(ctx context.Context, cc *rpc.ClientConn, kind int, opts ...rpc.CallOption) error {
	if kind < 0 {
		return cc.Invoke(ctx, unaryMethod, wrapperspb.String("x"), new(msg), opts...)
	}
	desc := &echoDesc.Streams[kind]
	st, err := cc.NewStream(ctx, desc, "/test.v1.Echo/"+desc.StreamName, opts...)
	if err != nil {
		return err
	}
	_ = st.SendMsg(wrapperspb.String("x"))
	_ = st.CloseSend()
	if err := st.RecvMsg(new(msg)); err != nil {
		return err
	}
	if desc.ServerStreams {
		_ = st.RecvMsg(new(msg))
	}
	return nil
}

func TestEarlyIdempotencyGates(t *testing.T) {
	for kind := -1; kind < len(echoDesc.Streams); kind++ {
		for _, clientMarked := range []bool{false, true} {
			for _, serverMarked := range []bool{false, true} {
				t.Run(fmt.Sprintf("kind=%d/client=%t/server=%t", kind, clientMarked, serverMarked), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						var called atomic.Int32
						cc, d := handshakeEnv(t, serverMarked, func(context.Context) { called.Add(1) })
						done := make(chan error, 1)
						go func() { done <- gatedCall(t.Context(), cc, kind, rpc.Idempotent(true), rpc.Idempotent(clientMarked)) }()
						synctest.Wait()
						want := int32(0)
						if clientMarked && serverMarked {
							want = 1
						}
						if called.Load() != want {
							t.Fatalf("early handlers = %d, want %d", called.Load(), want)
						}
						if !clientMarked && d.opens.Load() != 0 {
							t.Fatal("unmarked call opened an early stream")
						}
						close(d.ready)
						if err := <-done; err != nil {
							t.Fatal(err)
						}
						if called.Load() != 1 {
							t.Fatal("handler did not run exactly once")
						}
					})
				})
			}
		}
	}
}

func TestEarlyConcurrentCallsAndDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var called atomic.Int32
		remaining := make(chan time.Duration, 4)
		cc, d := handshakeEnv(t, true, func(ctx context.Context) {
			called.Add(1)
			if deadline, ok := ctx.Deadline(); ok {
				remaining <- time.Until(deadline)
			}
		})
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- gatedCall(ctx, cc, -1) }()
		synctest.Wait()
		if err := gatedCall(t.Context(), cc, -1, rpc.Idempotent(true)); err != nil {
			t.Fatal(err)
		}
		if called.Load() != 1 {
			t.Fatal("ordinary call bypassed the handshake gate")
		}
		time.Sleep(3 * time.Second) // virtual time
		close(d.ready)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if got := <-remaining; got != 7*time.Second {
			t.Fatalf("server deadline has %v left", got)
		}
	})
}

func TestEarlyHandshakeDeadline(t *testing.T) {
	for _, clientMarked := range []bool{false, true} {
		t.Run(fmt.Sprint(clientMarked), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var called atomic.Bool
				cc, _ := handshakeEnv(t, false, func(context.Context) { called.Store(true) })
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				err := gatedCall(ctx, cc, -1, rpc.Idempotent(clientMarked))
				wantCode(t, err, codes.DeadlineExceeded, "handshake deadline")
				synctest.Wait()
				if called.Load() {
					t.Fatal("handler ran past the handshake deadline")
				}
			})
		})
	}
}
