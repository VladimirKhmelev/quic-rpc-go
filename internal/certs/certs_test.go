package certs_test

import (
	"crypto/tls"
	"net"
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/internal/certs"
)

// handshake runs a TLS handshake between server and client over a pipe and
// returns the client's error.
func handshake(server, client *tls.Config) error {
	sc, cc := net.Pipe()
	defer func() { _ = sc.Close(); _ = cc.Close() }()
	done := make(chan error, 1)
	go func() { done <- tls.Server(sc, server).Handshake() }()
	err := tls.Client(cc, client).Handshake()
	if err != nil {
		_ = cc.Close()
	}
	<-done
	return err
}

func TestHandshake(t *testing.T) {
	p, err := certs.New("localhost", "127.0.0.1", "::1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := handshake(p.Server, p.Client); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	leaf := p.Server.Certificates[0].Leaf
	if len(leaf.IPAddresses) != 2 || len(leaf.DNSNames) != 1 {
		t.Fatalf("certificate for %v and %v, want localhost, 127.0.0.1 and ::1", leaf.DNSNames, leaf.IPAddresses)
	}
}

func TestClientTrustsOnlyItsCA(t *testing.T) {
	a, err := certs.New("localhost")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := certs.New("localhost")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := handshake(a.Server, b.Client); err == nil {
		t.Fatal("a client trusted a server certificate of another CA")
	}
}

func TestNeedsDNSName(t *testing.T) {
	if _, err := certs.New("127.0.0.1"); err == nil {
		t.Fatal("New accepted hosts without a DNS name")
	}
}
