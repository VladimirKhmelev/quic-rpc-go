package rpcquic_test

import (
	"crypto/tls"
	"reflect"
	"testing"

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
