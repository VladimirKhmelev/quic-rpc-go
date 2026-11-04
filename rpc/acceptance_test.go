package rpc_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/call"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

func TestConcurrentRPCsUseOneConnection(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		const calls = 32
		l, d := f(t, calls)
		started, release := make(chan struct{}, calls), make(chan struct{})
		releaseCalls := sync.OnceFunc(func() { close(release) })
		defer releaseCalls()
		srv := rpc.NewServer()
		srv.RegisterService(&echoDesc, &testServer{unary: func(ctx context.Context, in *msg) (*msg, error) {
			started <- struct{}{}
			select {
			case <-release:
				return in, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}})
		served := make(chan error, 1)
		go func() { served <- srv.Serve(l) }()
		var streams atomic.Int32
		dialer := &countingDialer{d: d, wrap: func(c transport.Conn) transport.Conn {
			return &countingConn{Conn: c, opens: &streams}
		}}
		cc := rpc.NewClient(dialer)
		t.Cleanup(func() {
			_ = cc.Close()
			srv.Stop()
			if err := <-served; err != nil {
				t.Error(err)
			}
		})
		ctx := transporttest.Context(t)
		results := make(chan error, calls)
		for i := range calls {
			go func() {
				value := fmt.Sprintf("call-%d", i)
				out := new(msg)
				err := cc.Invoke(ctx, unaryMethod, wrapperspb.String(value), out)
				if err == nil && out.Value != value {
					err = fmt.Errorf("response %q, want %q", out.Value, value)
				}
				results <- err
			}()
		}
		for range calls {
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("calls did not run concurrently:", ctx.Err())
			}
		}
		if dialer.dials.Load() != 1 || streams.Load() != calls {
			t.Fatalf("%d concurrent RPCs: %d dials, %d streams", calls, dialer.dials.Load(), streams.Load())
		}
		releaseCalls()
		for range calls {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		if err := invokeEcho(ctx, cc); err != nil {
			t.Fatal("connection did not survive completed calls:", err)
		}
		if dialer.dials.Load() != 1 || streams.Load() != calls+1 {
			t.Fatalf("follow-up RPC: %d dials, %d streams", dialer.dials.Load(), streams.Load())
		}
	})
}

// Run the actual client and server in separate virtual-clock bubbles. The
// captured request is replayed unchanged after moving the server's wall clock
// a day ahead or behind the client's; no production clock hook is needed.
func TestDeadlineWithClockSkew(t *testing.T) {
	const timeout = 5 * time.Second
	var wire []byte
	var clientNow time.Time
	synctest.Test(t, func(t *testing.T) {
		time.Sleep(48 * time.Hour)
		clientNow = time.Now()
		l, d := transporttest.Mem(t, 10)
		reply, err := call.AppendMessage(nil, wrapperspb.String("ok"))
		if err != nil {
			t.Fatal(err)
		}
		reply, err = call.AppendTrailers(reply, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		captured := make(chan []byte, 1)
		go func() {
			conn, err := l.Accept(t.Context())
			if err != nil {
				t.Error(err)
				return
			}
			st, err := conn.AcceptStream(t.Context())
			if err != nil {
				t.Error(err)
				return
			}
			data, err := io.ReadAll(st)
			if err != nil {
				t.Error(err)
				return
			}
			captured <- data
			_, _ = st.Write(reply)
			_ = st.Close()
		}()
		cc := rpc.NewClient(d)
		defer func() { _ = cc.Close() }()
		ctx, cancel := context.WithTimeout(t.Context(), timeout)
		defer cancel()
		if err := invokeEcho(ctx, cc); err != nil {
			t.Fatal(err)
		}
		wire = <-captured
	})
	for _, skew := range []time.Duration{-24 * time.Hour, 24 * time.Hour} {
		t.Run(skew.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				time.Sleep(48*time.Hour + skew)
				if time.Since(clientNow) != skew {
					t.Fatal("server and client clocks did not differ by the requested skew")
				}
				left := make(chan time.Duration, 1)
				handlerErr := make(chan error, 1)
				l, d := transporttest.Mem(t, 10)
				srv := rpc.NewServer()
				srv.RegisterService(&echoDesc, &testServer{unary: func(ctx context.Context, _ *msg) (*msg, error) {
					deadline, ok := ctx.Deadline()
					if !ok {
						left <- -1
					} else {
						left <- time.Until(deadline)
					}
					<-ctx.Done()
					handlerErr <- ctx.Err()
					return nil, ctx.Err()
				}})
				served := make(chan error, 1)
				go func() { served <- srv.Serve(l) }()
				conn, err := d.Dial(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					_ = conn.CloseWithError(transport.NoError, "")
					srv.Stop()
					if err := <-served; err != nil {
						t.Error(err)
					}
				}()
				st, err := conn.OpenStream(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				start := time.Now()
				_, _ = st.Write(wire)
				_ = st.Close()
				if got := <-left; got != timeout {
					t.Fatalf("server timeout with clock skew %v: %v, want %v", skew, got, timeout)
				}
				_, _ = io.Copy(io.Discard, st)
				if err := <-handlerErr; !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("handler error: %v", err)
				}
				if elapsed := time.Since(start); elapsed != timeout {
					t.Fatalf("server canceled after %v, want %v", elapsed, timeout)
				}
			})
		})
	}
}
