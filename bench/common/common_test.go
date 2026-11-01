package common_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/certs"
)

func TestMain(m *testing.M) {
	// The test machine may not allow the UDP buffers quic-go asks for.
	_ = os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	os.Exit(m.Run())
}

// Status codes, numbered as in gRPC.
const (
	codeCanceled         = 1
	codeInvalidArgument  = 3
	codeDeadlineExceeded = 4
	codeUnavailable      = 14
)

type env struct {
	server common.Server
	client common.Client
	// curve is the key exchange of the last handshake of the client;
	// handshakes counts them.
	curve       atomic.Uint32
	handshakes  atomic.Int32
	connections int
}

// forEach runs fn on a server and a client of every transport over
// loopback.
func forEach(t *testing.T, fn func(t *testing.T, e *env)) {
	for _, tr := range common.Transports {
		t.Run(string(tr), func(t *testing.T) {
			p, err := certs.New("localhost", "127.0.0.1")
			if err != nil {
				t.Fatal(err)
			}
			e := &env{}
			options, err := (common.ClientOptions{}).Resolve(tr)
			if err != nil {
				t.Fatal(err)
			}
			e.connections = options.PoolSize
			srv, err := common.Listen(tr, "127.0.0.1:0", p.Server)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(srv.Stop)
			e.server = srv
			clientTLS := p.Client.Clone()
			clientTLS.VerifyConnection = func(cs tls.ConnectionState) error {
				e.curve.Store(uint32(cs.CurveID))
				e.handshakes.Add(1)
				return nil
			}
			if e.client, err = common.Dial(tr, srv.Addr().String(), clientTLS); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = e.client.Close() })
			fn(t, e)
		})
	}
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return c
}

func TestConnect(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		if err := e.client.Connect(ctx(t)); err != nil {
			t.Fatal(err)
		}
		if got := tls.CurveID(e.curve.Load()); got != tls.X25519MLKEM768 {
			t.Errorf("key exchange %v, want %v", got, tls.X25519MLKEM768)
		}
		if n, err := e.client.Call(ctx(t), nil, 10); err != nil || n != 10 {
			t.Fatalf("call after Connect: %d, %v", n, err)
		}
		// The call used the connection Connect set up.
		if n := e.handshakes.Load(); int(n) != e.connections {
			t.Errorf("%d handshakes for Connect and a call, want %d", n, e.connections)
		}
	})
}

func TestCall(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		// Without Connect: the first call connects.
		for _, tt := range []struct{ req, resp int }{{0, 0}, {100, 1000}, {1000, 100}, {1 << 20, 1 << 20}, {0, common.MaxResponse}} {
			n, err := e.client.Call(ctx(t), make([]byte, tt.req), tt.resp)
			if err != nil || n != tt.resp {
				t.Errorf("Call of %d bytes for %d: got %d, %v", tt.req, tt.resp, n, err)
			}
		}
		for _, size := range []int{-1, common.MaxResponse + 1} {
			if _, err := e.client.Call(ctx(t), nil, size); common.Code(err) != codeInvalidArgument {
				t.Errorf("Call for %d bytes: %v, want InvalidArgument", size, err)
			}
		}
	})
}

func TestDeadline(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()
		if _, err := e.client.Call(expired, nil, 1); common.Code(err) != codeDeadlineExceeded {
			t.Fatalf("Call with an expired deadline: %v, want DeadlineExceeded", err)
		}
	})
}

func TestEcho(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		s, err := e.client.Echo(ctx(t))
		if err != nil {
			t.Fatal(err)
		}
		const n = 20
		got := make(chan error, 1)
		go func() {
			for want := range uint64(n) {
				seq, size, err := s.Recv()
				if err != nil || seq != want || size != 200 {
					got <- errors.Join(err, errors.New("wrong echo"))
					return
				}
			}
			_, _, err := s.Recv()
			if !errors.Is(err, io.EOF) {
				got <- errors.Join(err, errors.New("no io.EOF after the last echo"))
				return
			}
			got <- nil
		}()
		for seq := range uint64(n) {
			if err := s.Send(seq, make([]byte, 200)); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.CloseSend(); err != nil {
			t.Fatal(err)
		}
		if err := <-got; err != nil {
			t.Fatal(err)
		}
	})
}

func TestConcurrentCalls(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		var wg sync.WaitGroup
		var failed atomic.Int32
		for i := range 100 {
			wg.Go(func() {
				if n, err := e.client.Call(ctx(t), make([]byte, i), i*10); err != nil || n != i*10 {
					failed.Add(1)
				}
			})
		}
		wg.Wait()
		if n := failed.Load(); n > 0 {
			t.Fatalf("%d of 100 concurrent calls failed", n)
		}
	})
}

func TestClose(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		if _, err := e.client.Call(ctx(t), nil, 1); err != nil {
			t.Fatal(err)
		}
		if err := e.client.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := e.client.Call(ctx(t), nil, 1); common.Code(err) != codeCanceled {
			t.Fatalf("Call after Close: %v, want Canceled", err)
		}
	})
}

func TestServerStop(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		if _, err := e.client.Call(ctx(t), nil, 1); err != nil {
			t.Fatal(err)
		}
		e.server.Stop()
		short, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		if _, err := e.client.Call(short, nil, 1); common.Code(err) != codeUnavailable && common.Code(err) != codeDeadlineExceeded {
			t.Fatalf("Call after the server stopped: %v, want Unavailable or DeadlineExceeded", err)
		}
	})
}

func TestParseTransport(t *testing.T) {
	for _, tr := range common.Transports {
		if got, err := common.ParseTransport(string(tr)); err != nil || got != tr {
			t.Errorf("ParseTransport(%q) = %q, %v", tr, got, err)
		}
	}
	if _, err := common.ParseTransport("h3"); err == nil {
		t.Error("ParseTransport took an unknown transport")
	}
	if common.Code(nil) != 0 || common.Code(errors.New("x")) != 2 {
		t.Error("Code of nil or of a plain error")
	}
}
