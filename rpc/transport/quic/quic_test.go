package rpcquic_test

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
	rpcquic "github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/quic"
)

func listen(t *testing.T, tlsConf *tls.Config, conf *quic.Config) *rpcquic.Listener {
	t.Helper()
	// quic-go warns when the kernel limits UDP buffers; that is irrelevant here.
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	l, err := rpcquic.Listen("127.0.0.1:0", tlsConf, conf)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func TestConformance(t *testing.T) {
	transporttest.Run(t, transporttest.QUIC)
}

func TestTLSConfigRequired(t *testing.T) {
	if _, err := rpcquic.Listen("127.0.0.1:0", nil, nil); err == nil {
		t.Error("Listen without a TLS config succeeded")
	}
	d := &rpcquic.Dialer{Addr: "127.0.0.1:1"}
	if _, err := d.Dial(transporttest.Context(t)); err == nil {
		t.Error("Dial without a TLS config succeeded")
	}
}

func TestALPN(t *testing.T) {
	serverTLS, clientTLS := transporttest.TLSConfigs(t)

	foreign := serverTLS.Clone()
	foreign.NextProtos = []string{"h3"}
	if _, err := rpcquic.Listen("127.0.0.1:0", foreign, nil); err == nil {
		t.Error("Listen accepted a TLS config with a foreign ALPN")
	}

	explicit := serverTLS.Clone()
	explicit.NextProtos = []string{rpcquic.ALPN}
	l := listen(t, explicit, nil)

	foreignClient := clientTLS.Clone()
	foreignClient.NextProtos = []string{"h3"}
	d := &rpcquic.Dialer{Addr: l.Addr().String(), TLSConfig: foreignClient}
	if _, err := d.Dial(transporttest.Context(t)); err == nil {
		t.Error("Dial accepted a TLS config with a foreign ALPN")
	}

	// A peer speaking another protocol fails the handshake.
	h3Client := clientTLS.Clone()
	h3Client.NextProtos = []string{"h3"}
	if c, err := quic.DialAddr(transporttest.Context(t), l.Addr().String(), h3Client, nil); err == nil {
		_ = c.CloseWithError(0, "")
		t.Error("a client with ALPN h3 connected")
	}
}

func TestUntrustedServerCertificate(t *testing.T) {
	serverTLS, _ := transporttest.TLSConfigs(t)
	_, otherClientTLS := transporttest.TLSConfigs(t)
	l := listen(t, serverTLS, nil)
	d := &rpcquic.Dialer{Addr: l.Addr().String(), TLSConfig: otherClientTLS}
	if _, err := d.Dial(transporttest.Context(t)); err == nil {
		t.Fatal("Dial trusted a certificate signed by another CA")
	}
}

func TestConfigsAreNotModified(t *testing.T) {
	serverTLS, clientTLS := transporttest.TLSConfigs(t)
	serverQUIC := &quic.Config{MaxIncomingStreams: 7}
	clientQUIC := &quic.Config{MaxIncomingStreams: 7}
	wantServerQUIC, wantClientQUIC := *serverQUIC, *clientQUIC

	l := listen(t, serverTLS, serverQUIC)
	d := &rpcquic.Dialer{Addr: l.Addr().String(), TLSConfig: clientTLS, QUICConfig: clientQUIC}
	c, err := d.Dial(transporttest.Context(t))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = c.CloseWithError(transport.NoError, "")

	if serverTLS.NextProtos != nil || clientTLS.NextProtos != nil {
		t.Errorf("NextProtos were modified: server %q, client %q", serverTLS.NextProtos, clientTLS.NextProtos)
	}
	if !reflect.DeepEqual(*serverQUIC, wantServerQUIC) || !reflect.DeepEqual(*clientQUIC, wantClientQUIC) {
		t.Errorf("QUIC configs were modified: server %+v, client %+v", *serverQUIC, *clientQUIC)
	}
}

// TestKeepAlive: by default an idle client connection stays open past the
// idle timeout; with keepalives turned off, quic-go closes it.
func TestKeepAlive(t *testing.T) {
	serverTLS, clientTLS := transporttest.TLSConfigs(t)
	l := listen(t, serverTLS, nil)
	const idle = 300 * time.Millisecond
	for _, tt := range []struct {
		name   string
		period time.Duration
		open   bool
	}{{"default", 0, true}, {"turned off", -1, false}} {
		t.Run(tt.name, func(t *testing.T) {
			d := &rpcquic.Dialer{Addr: l.Addr().String(), TLSConfig: clientTLS,
				QUICConfig: &quic.Config{MaxIdleTimeout: idle, KeepAlivePeriod: tt.period}}
			c, err := d.Dial(transporttest.Context(t))
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer func() { _ = c.CloseWithError(transport.NoError, "") }()
			timer := time.NewTimer(5 * idle)
			defer timer.Stop()
			select {
			case <-c.Context().Done():
				if tt.open {
					t.Fatalf("idle connection closed: %v", context.Cause(c.Context()))
				}
				if _, ok := errors.AsType[*quic.IdleTimeoutError](context.Cause(c.Context())); !ok {
					t.Fatalf("idle connection closed: %v, want an idle timeout", context.Cause(c.Context()))
				}
			case <-timer.C:
				if !tt.open {
					t.Fatalf("idle connection without keepalives open after %v, the idle timeout is %v", 5*idle, idle)
				}
			}
		})
	}
}

// TestNewListenerChecksTLS: NewListener adjusts and checks the TLS config
// as Listen does.
func TestNewListenerChecksTLS(t *testing.T) {
	serverTLS, _ := transporttest.TLSConfigs(t)
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tr := &quic.Transport{Conn: pc}
	defer func() { _ = tr.Close(); _ = pc.Close() }()
	if _, err := rpcquic.NewListener(tr, nil, nil); err == nil {
		t.Error("NewListener without a TLS config succeeded")
	}
	foreign := serverTLS.Clone()
	foreign.NextProtos = []string{"h3"}
	if _, err := rpcquic.NewListener(tr, foreign, nil); err == nil {
		t.Error("NewListener accepted a TLS config with a foreign ALPN")
	}
}

// TestServerRefusesUniStreams: a server made by either Listen or
// NewListener allows no unidirectional streams, even to a client that is
// not ours.
func TestServerRefusesUniStreams(t *testing.T) {
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	serverTLS, clientTLS := transporttest.TLSConfigs(t)
	pc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	tr := &quic.Transport{Conn: pc}
	defer func() { _ = tr.Close(); _ = pc.Close() }()
	fromTransport, err := rpcquic.NewListener(tr, serverTLS, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, l := range map[string]*rpcquic.Listener{"Listen": listen(t, serverTLS, nil), "NewListener": fromTransport} {
		ctx := transporttest.Context(t)
		tc := clientTLS.Clone()
		tc.NextProtos = []string{rpcquic.ALPN}
		qc, err := quic.DialAddr(ctx, l.Addr().String(), tc, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := qc.OpenUniStream(); err == nil {
			t.Errorf("%s: the server allowed a unidirectional stream", name)
		}
		_ = qc.CloseWithError(0, "")
	}
}
