package rpcquic_test

import (
	"crypto/hkdf"
	"crypto/sha256"
	"crypto/tls"
	"log"
	"net"
	"os"

	"github.com/quic-go/quic-go"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	rpcquic "github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/quic"
)

// A server that restarts listens with a stateless reset key that is the
// same on every start. Here the key is derived from a secret in the
// environment; the secret must stay secret, as anyone who knows the key can
// close the server's connections.
func ExampleNewListener() {
	var serverTLS *tls.Config // with the server certificate

	secret := os.Getenv("QUIC_RESET_SECRET")
	if secret == "" {
		log.Fatal("QUIC_RESET_SECRET is not set")
	}
	key, err := hkdf.Key(sha256.New, []byte(secret), nil, "qrpc stateless reset", len(quic.StatelessResetKey{}))
	if err != nil {
		log.Fatal(err)
	}
	pc, err := net.ListenUDP("udp", &net.UDPAddr{Port: 4433})
	if err != nil {
		log.Fatal(err)
	}
	tr := &quic.Transport{Conn: pc, StatelessResetKey: (*quic.StatelessResetKey)(key)}
	l, err := rpcquic.NewListener(tr, serverTLS, nil)
	if err != nil {
		log.Fatal(err)
	}

	srv := rpc.NewServer()
	// srv.RegisterService(...)
	if err := srv.Serve(l); err != nil {
		log.Fatal(err)
	}
	// After srv.Stop: tr.Close and pc.Close.
}
