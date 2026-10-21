package h2

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/VladimirKhmelev/quic-rpc-go/internal/certs"
)

// TestNoProxy dials past a proxy that the environment names. The proxy is
// dead, so the call reaches the server only if it goes straight there. It
// needs an address other than loopback, which proxies never take.
func TestNoProxy(t *testing.T) {
	var ip net.IP
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			ip = n.IP
			break
		}
	}
	if ip == nil {
		t.Skip("no IPv4 address but loopback")
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	t.Setenv("NO_PROXY", "")
	pair, err := certs.New("localhost", ip.String())
	if err != nil {
		t.Fatal(err)
	}
	lis, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(pair.Server)
	go func() { _ = srv.Serve(lis) }()
	defer srv.Stop()
	cc, err := Dial(lis.Addr().String(), pair.Client)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cc.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// The server has no services: reaching it means Unimplemented.
	err = cc.Invoke(ctx, "/test.Nothing/Call", &emptypb.Empty{}, &emptypb.Empty{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("call: %v, want Unimplemented from the server", err)
	}
}
