package rpcquic_test

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
	rpcquic "github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/quic"
)

// server is a QUIC server on a fixed UDP port that can crash and start
// again.
type server struct {
	t    *testing.T
	tls  *tls.Config
	key  *quic.StatelessResetKey
	addr *net.UDPAddr
	tr   *quic.Transport
	l    *rpcquic.Listener
}

func (s *server) start() {
	s.t.Helper()
	pc, err := net.ListenUDP("udp", s.addr)
	if err != nil {
		s.t.Fatal(err)
	}
	s.addr = pc.LocalAddr().(*net.UDPAddr) //nolint:errcheck // a UDP socket has a UDP address
	s.tr = &quic.Transport{Conn: pc, StatelessResetKey: s.key}
	if s.l, err = rpcquic.NewListener(s.tr, s.tls, nil); err != nil {
		s.t.Fatal(err)
	}
	tr := s.tr
	s.t.Cleanup(func() { _ = tr.Close(); _ = pc.Close() })
}

// crash ends the server as a killed process ends: its connections vanish
// without a CONNECTION_CLOSE, and the port is free again.
func (s *server) crash() {
	_ = s.tr.Close()
	_ = s.tr.Conn.Close()
}

// roundTrip sends a stream from c to the server and reads it there.
func (s *server) roundTrip(ctx context.Context, c transport.Conn) {
	s.t.Helper()
	st, err := c.OpenStream(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := st.Write([]byte("ping")); err != nil {
		s.t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		s.t.Fatal(err)
	}
	sc, err := s.l.Accept(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	ss, err := sc.AcceptStream(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	if b, err := io.ReadAll(ss); err != nil || string(b) != "ping" {
		s.t.Fatalf("server read %q, %v", b, err)
	}
}

// TestStatelessReset crashes a server and starts it again on the same port.
// With the same stateless reset key, the client's connection closes at the
// first packet it sends to the new server; without a key it stays open,
// until the idle timeout.
func TestStatelessReset(t *testing.T) {
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	serverTLS, clientTLS := transporttest.TLSConfigs(t)
	key := quic.StatelessResetKey([]byte("a key that outlives the server.."))
	for _, tt := range []struct {
		name string
		key  *quic.StatelessResetKey
	}{{"with key", &key}, {"without key", nil}} {
		t.Run(tt.name, func(t *testing.T) {
			s := &server{t: t, tls: serverTLS, key: tt.key, addr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}}
			s.start()
			ctx := transporttest.Context(t)
			c, err := (&rpcquic.Dialer{Addr: s.addr.String(), TLSConfig: clientTLS}).Dial(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.CloseWithError(transport.NoError, "") }()
			s.roundTrip(ctx, c)

			s.crash()
			s.start()
			st, err := c.OpenStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = st.Write([]byte("to the new server")) // may fail once the reset is in
			timer := time.NewTimer(time.Second)
			defer timer.Stop()
			select {
			case <-c.Context().Done():
				_, isReset := errors.AsType[*quic.StatelessResetError](context.Cause(c.Context()))
				if tt.key == nil || !isReset {
					t.Fatalf("connection closed: %v", context.Cause(c.Context()))
				}
			case <-timer.C:
				if tt.key != nil {
					t.Fatal("connection still open 1 s after its packets reached the new server")
				}
			}
			if tt.key != nil {
				// The new server takes new connections.
				c, err := (&rpcquic.Dialer{Addr: s.addr.String(), TLSConfig: clientTLS}).Dial(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = c.CloseWithError(transport.NoError, "") }()
				s.roundTrip(ctx, c)
			}
		})
	}
}
