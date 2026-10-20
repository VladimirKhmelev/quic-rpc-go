// Package certs issues the TLS certificates of the tests, the testbed and the
// benchmarks: a throwaway self-signed CA and a server certificate signed by
// it. Both transports get the same certificates, so TLS is the same for all
// variants.
package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// validFor covers the longest benchmark series with room to spare.
const validFor = 30 * 24 * time.Hour

// Pair holds the TLS configurations of both sides.
type Pair struct {
	// Server presents the server certificate.
	Server *tls.Config
	// Client trusts only the CA of this pair and expects the first DNS name
	// of the hosts as the server name.
	Client *tls.Config
}

// KeyExchange is the TLS key exchange of every variant, set explicitly on
// both sides: X25519MLKEM768, the default of Go since 1.24. Its
// ClientHello is larger than 1 KB and takes two packets, which matters for
// connection setup under losses.
var KeyExchange = []tls.CurveID{tls.X25519MLKEM768}

// Bundle is an issued CA and server certificate in PEM, as files can carry
// them from the process that issued them to a server and its clients.
type Bundle struct {
	CA   []byte // the CA certificate
	Cert []byte // the server certificate followed by the CA certificate
	Key  []byte // the server key, PKCS #8
}

// Files of a bundle in a directory.
const (
	caFile   = "ca.pem"
	certFile = "server.pem"
	keyFile  = "server.key"
)

// New issues a CA and a server certificate valid for hosts: DNS names and
// IP addresses. At least one host must be a DNS name.
func New(hosts ...string) (*Pair, error) {
	b, err := Issue(hosts...)
	if err != nil {
		return nil, err
	}
	return b.Pair()
}

// Issue issues a CA and a server certificate valid for hosts, as New does.
func Issue(hosts ...string) (*Bundle, error) {
	var dnsNames []string
	var ips []net.IP
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
		} else {
			dnsNames = append(dnsNames, h)
		}
	}
	if len(dnsNames) == 0 {
		return nil, errors.New("certs: hosts need at least one DNS name")
	}

	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("certs: generate CA key: %w", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "quic-rpc-go test CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("certs: create CA certificate: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("certs: parse CA certificate: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("certs: generate server key: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(validFor),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("certs: create server certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("certs: encode server key: %w", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return &Bundle{
		CA:   caPEM,
		Cert: append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), caPEM...),
		Key:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// Pair returns the TLS configurations of the bundle.
func (b *Bundle) Pair() (*Pair, error) {
	cert, err := tls.X509KeyPair(b.Cert, b.Key)
	if err != nil {
		return nil, fmt.Errorf("certs: %w", err)
	}
	if len(cert.Leaf.DNSNames) == 0 {
		return nil, errors.New("certs: the server certificate has no DNS name")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(b.CA) {
		return nil, errors.New("certs: no CA certificate")
	}
	return &Pair{
		Server: &tls.Config{
			Certificates:     []tls.Certificate{cert},
			MinVersion:       tls.VersionTLS13,
			CurvePreferences: KeyExchange,
		},
		Client: &tls.Config{
			RootCAs:          pool,
			ServerName:       cert.Leaf.DNSNames[0],
			MinVersion:       tls.VersionTLS13,
			CurvePreferences: KeyExchange,
		},
	}, nil
}

// Write stores the bundle in dir, the key readable by the owner only.
func (b *Bundle) Write(dir string) error {
	for name, data := range map[string][]byte{caFile: b.CA, certFile: b.Cert} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil { //nolint:gosec // certificates are public
			return fmt.Errorf("certs: %w", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, keyFile), b.Key, 0o600); err != nil {
		return fmt.Errorf("certs: %w", err)
	}
	return nil
}

// Load reads a bundle that Write stored in dir.
func Load(dir string) (*Bundle, error) {
	var b Bundle
	for name, data := range map[string]*[]byte{caFile: &b.CA, certFile: &b.Cert, keyFile: &b.Key} {
		var err error
		if *data, err = os.ReadFile(filepath.Join(dir, name)); err != nil { //nolint:gosec // a directory of certificates the caller names
			return nil, fmt.Errorf("certs: %w", err)
		}
	}
	return &b, nil
}
