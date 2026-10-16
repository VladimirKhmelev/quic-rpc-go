package transporttest

import (
	"testing"

	"github.com/quic-go/quic-go"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/mem"
	rpcquic "github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/quic"
)

// Mem is a Factory for the in-process transport. It works inside
// testing/synctest bubbles.
func Mem(t *testing.T, maxStreams int) (transport.Listener, transport.Dialer) {
	l := mem.Listen(mem.Options{MaxIncomingStreams: maxStreams})
	t.Cleanup(func() { _ = l.Close() })
	return l, l
}

// QUIC is a Factory for the QUIC transport on a loopback UDP port.
func QUIC(t *testing.T, maxStreams int) (transport.Listener, transport.Dialer) {
	// quic-go warns when the kernel limits UDP buffers; that is irrelevant here.
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	serverTLS, clientTLS := TLSConfigs(t)
	l, err := rpcquic.Listen("127.0.0.1:0", serverTLS, &quic.Config{MaxIncomingStreams: int64(maxStreams)})
	if err != nil {
		t.Fatalf("rpcquic.Listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, &rpcquic.Dialer{Addr: l.Addr().String(), TLSConfig: clientTLS}
}

// Transports lists the transports every RPC-level test runs on.
var Transports = []struct {
	Name    string
	Factory Factory
}{
	{"mem", Mem},
	{"quic", QUIC},
}
