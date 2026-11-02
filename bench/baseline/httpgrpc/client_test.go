package httpgrpc_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/baseline/httpgrpc"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/certs"
)

func testContext(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func wait(ctx context.Context, t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// Connect must complete the protocol setup without a request or extra PING.
// Repeated requests must use the same physical connection and HTTP version.
func TestVersionAndReuse(t *testing.T) {
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	for _, version := range []int{2, 3} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			ctx := testContext(t)
			pair, err := certs.New("localhost")
			if err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			srv, err := httpgrpc.Listen("127.0.0.1:0", pair.Server, version, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.ProtoMajor != version {
					t.Error("wrong server HTTP version")
				}
				w.Header().Set("X-Peer", r.RemoteAddr)
				_, _ = io.WriteString(w, "ok")
			}))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(srv.Stop)
			c, err := httpgrpc.NewClient(srv.Addr().String(), pair.Client, version)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			if err := c.Connect(ctx); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != 0 {
				t.Fatal("Connect issued a probe request")
			}
			peer := ""
			for range 3 {
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+srv.Addr().String()+"/", nil)
				if err != nil {
					t.Fatal(err)
				}
				res, err := (&http.Client{Transport: c}).Do(req)
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
				if err != nil || res.ProtoMajor != version {
					t.Fatalf("response: %s, %v", res.Proto, err)
				}
				if peer != "" && peer != res.Header.Get("X-Peer") {
					t.Fatal("request opened another connection")
				}
				peer = res.Header.Get("X-Peer")
			}
			if len(pair.Client.NextProtos) != 0 || len(pair.Server.NextProtos) != 0 {
				t.Fatal("mutated caller TLS config")
			}
		})
	}
}

func TestCloseAndServerStop(t *testing.T) {
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	for _, version := range []int{2, 3} {
		for _, closeClient := range []bool{false, true} {
			t.Run(strconv.Itoa(version)+"/client="+strconv.FormatBool(closeClient), func(t *testing.T) {
				ctx := testContext(t)
				pair, err := certs.New("localhost")
				if err != nil {
					t.Fatal(err)
				}
				started, ended := make(chan struct{}), make(chan struct{})
				srv, err := httpgrpc.Listen("127.0.0.1:0", pair.Server, version, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					close(started)
					<-r.Context().Done()
					close(ended)
				}))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(srv.Stop)
				c, err := httpgrpc.NewClient(srv.Addr().String(), pair.Client, version)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = c.Close() })
				done := make(chan error, 1)
				go func() {
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+srv.Addr().String()+"/", nil)
					if err == nil {
						_, err = (&http.Client{Transport: c}).Do(req)
					}
					done <- err
				}()
				wait(ctx, t, started)
				if closeClient {
					_ = c.Close()
				} else {
					srv.Stop()
				}
				wait(ctx, t, ended)
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("active request succeeded after close")
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if closeClient && !errors.Is(c.Connect(ctx), httpgrpc.ErrClosed) {
					t.Fatal("client reopened after Close")
				}
			})
		}
	}
}

func TestCancelHandshake(t *testing.T) {
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	for _, version := range []int{2, 3} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			ctx := testContext(t)
			pair, err := certs.New("localhost")
			if err != nil {
				t.Fatal(err)
			}
			started, release := make(chan struct{}, 4), make(chan struct{})
			defer close(release)
			pair.Server.GetConfigForClient = func(info *tls.ClientHelloInfo) (*tls.Config, error) {
				started <- struct{}{}
				select {
				case <-release:
					return nil, nil
				case <-info.Context().Done():
					return nil, info.Context().Err()
				}
			}
			srv, err := httpgrpc.Listen("127.0.0.1:0", pair.Server, version, http.NotFoundHandler())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(srv.Stop)
			c, err := httpgrpc.NewClient(srv.Addr().String(), pair.Client, version)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			waitCtx, cancel := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- c.Connect(waitCtx) }()
			wait(ctx, t, started)
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(c.Connect(ctx), httpgrpc.ErrClosed) {
				t.Fatal("closed client started dialing")
			}
		})
	}
}
