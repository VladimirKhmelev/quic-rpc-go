package rpc_test

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
	rpcquic "github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/quic"
)

// switchDialer lets a test point a ClientConn at another server.
type switchDialer struct {
	target atomic.Pointer[transport.Dialer]
}

func (s *switchDialer) Dial(ctx context.Context) (transport.Conn, error) {
	return (*s.target.Load()).Dial(ctx)
}

func TestRedialAfterConnectionLoss(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		start := func() (*rpc.Server, transport.Dialer, chan error) {
			l, d := f(t, 10)
			srv := rpc.NewServer()
			srv.RegisterService(&echoDesc, &testServer{})
			served := make(chan error, 1)
			go func() { served <- srv.Serve(l) }()
			return srv, d, served
		}
		first, d1, served1 := start()
		var dialer switchDialer
		dialer.target.Store(&d1)
		cc := rpc.NewClient(&dialer)
		defer func() { _ = cc.Close() }()

		ctx := transporttest.Context(t)
		if err := cc.Invoke(ctx, unaryMethod, wrapperspb.String("1"), new(msg)); err != nil {
			t.Fatalf("first call: %v", err)
		}
		first.Stop()
		<-served1

		second, d2, served2 := start()
		defer func() { second.Stop(); <-served2 }()
		dialer.target.Store(&d2)
		// The lost connection is noticed on the next call, which dials again.
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var err error
		for range 50 {
			if err = cc.Invoke(ctx, unaryMethod, wrapperspb.String("2"), new(msg)); err == nil {
				return
			}
			if status.Code(err) != codes.Unavailable {
				break
			}
		}
		t.Fatalf("calls after the server came back: %v", err)
	})
}

// TestServerRestart crashes a QUIC server and starts it again on the same
// port with the same stateless reset key. The client is not told of the
// crash: its first call goes out on the old connection and fails with
// Unavailable as soon as the new server answers it with a reset, not after
// the idle timeout; the next call dials again.
func TestServerRestart(t *testing.T) {
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	serverTLS, clientTLS := transporttest.TLSConfigs(t)
	key := quic.StatelessResetKey([]byte("a key that outlives the server.."))
	addr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	start := func() (*quic.Transport, chan error) {
		pc, err := net.ListenUDP("udp", addr)
		if err != nil {
			t.Fatal(err)
		}
		addr = pc.LocalAddr().(*net.UDPAddr) //nolint:errcheck // a UDP socket has a UDP address
		tr := &quic.Transport{Conn: pc, StatelessResetKey: &key}
		l, err := rpcquic.NewListener(tr, serverTLS, nil)
		if err != nil {
			t.Fatal(err)
		}
		srv := rpc.NewServer()
		srv.RegisterService(&echoDesc, &testServer{})
		served := make(chan error, 1)
		go func() { served <- srv.Serve(l) }()
		t.Cleanup(func() { srv.Stop(); _ = tr.Close(); _ = pc.Close() })
		return tr, served
	}
	tr, served := start()
	cc := rpc.NewClient(&rpcquic.Dialer{Addr: addr.String(), TLSConfig: clientTLS})
	defer func() { _ = cc.Close() }()
	ctx := transporttest.Context(t)
	if err := cc.Invoke(ctx, unaryMethod, wrapperspb.String("1"), new(msg)); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// As a killed process: the connections vanish without a word.
	_ = tr.Close()
	_ = tr.Conn.Close()
	<-served
	start()
	began := time.Now()
	err := cc.Invoke(ctx, unaryMethod, wrapperspb.String("2"), new(msg))
	wantCode(t, err, codes.Unavailable, "call on the connection to the crashed server")
	if d := time.Since(began); d > time.Second {
		t.Errorf("the call failed after %v", d)
	}
	if err := cc.Invoke(ctx, unaryMethod, wrapperspb.String("3"), new(msg)); err != nil {
		t.Fatalf("call after the restart: %v", err)
	}
}

func TestClientConnClose(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		started := make(chan struct{})
		e := newEnv(t, f, &testServer{unary: func(ctx context.Context, _ *msg) (*msg, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}})
		result := make(chan error, 1)
		go func() {
			_, err := e.unary(transporttest.Context(t), "x")
			result <- err
		}()
		<-started
		if err := e.cc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		wantCode(t, <-result, codes.Canceled, "call in flight during Close")
		_, err := e.unary(transporttest.Context(t), "x")
		wantCode(t, err, codes.Canceled, "call after Close")
		if err := e.cc.Close(); err == nil {
			t.Fatal("second Close succeeded")
		}
	})
}
