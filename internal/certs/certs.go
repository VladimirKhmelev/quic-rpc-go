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
	"errors"
	"fmt"
	"math/big"
	"net"
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

// New issues a CA and a server certificate valid for hosts: DNS names and
// IP addresses. At least one host must be a DNS name.
func New(hosts ...string) (*Pair, error) {
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
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("certs: parse server certificate: %w", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return &Pair{
		Server: &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{der, caDER}, PrivateKey: key, Leaf: leaf}},
			MinVersion:   tls.VersionTLS13,
		},
		Client: &tls.Config{
			RootCAs:    pool,
			ServerName: dnsNames[0],
			MinVersion: tls.VersionTLS13,
		},
	}, nil
}
