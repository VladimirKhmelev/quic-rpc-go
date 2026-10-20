package certs_test

import (
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"slices"
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

// TestKeyExchange checks that the handshake uses the key exchange both sides
// insist on.
func TestKeyExchange(t *testing.T) {
	p, err := certs.New("localhost")
	if err != nil {
		t.Fatal(err)
	}
	var got tls.CurveID
	client := p.Client.Clone()
	client.VerifyConnection = func(cs tls.ConnectionState) error { got = cs.CurveID; return nil }
	if err := handshake(p.Server, client); err != nil {
		t.Fatal(err)
	}
	if got != tls.X25519MLKEM768 {
		t.Fatalf("key exchange %v, want %v", got, tls.X25519MLKEM768)
	}
	// Set explicitly, not left to the defaults of Go, which may change.
	for side, c := range map[string]*tls.Config{"server": p.Server, "client": p.Client} {
		if !slices.Equal(c.CurvePreferences, []tls.CurveID{tls.X25519MLKEM768}) {
			t.Errorf("%s key exchanges %v", side, c.CurvePreferences)
		}
	}
}

// TestBundleFiles carries a bundle through files, as from the process that
// issues it to a server and a client.
func TestBundleFiles(t *testing.T) {
	b, err := certs.Issue("localhost", "10.0.2.2")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := b.Write(dir); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "server.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("server.key: %v, %v", fi, err)
	}
	loaded, err := certs.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loaded.Pair()
	if err != nil {
		t.Fatal(err)
	}
	if p.Client.ServerName != "localhost" {
		t.Errorf("server name %q", p.Client.ServerName)
	}
	if err := handshake(p.Server, p.Client); err != nil {
		t.Fatalf("handshake with loaded files: %v", err)
	}
	if _, err := certs.Load(t.TempDir()); err == nil {
		t.Error("Load of an empty directory succeeded")
	}
}
