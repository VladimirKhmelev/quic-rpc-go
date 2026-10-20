// Package common is what every transport of the benchmarks implements
// (GEN-1): a client that makes the calls of the load generator and a server
// that answers them, both over the service of proto/bench/v1, so that one
// scenario runs on any transport unchanged.
//
// Transports: quic-rpc-go over QUIC and grpc-go over HTTP/2 with one
// connection. Both use the TLS configurations they are given; the
// benchmarks give them the same ones (internal/certs).
package common

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
)

// Transport names a variant of the comparison.
type Transport string

// The transports.
const (
	// QUIC is quic-rpc-go over QUIC.
	QUIC Transport = "quic"
	// GRPCH2 is grpc-go over HTTP/2, one TCP connection per client.
	GRPCH2 Transport = "grpc-h2"
)

// Transports lists every transport.
var Transports = []Transport{QUIC, GRPCH2}

// ParseTransport returns the transport named s.
func ParseTransport(s string) (Transport, error) {
	for _, t := range Transports {
		if string(t) == s {
			return t, nil
		}
	}
	return "", fmt.Errorf("transport %q: want %s or %s", s, QUIC, GRPCH2)
}

// Client makes calls to one server over one connection. It is safe for
// concurrent use.
type Client interface {
	// Connect sets up the connection: for QUIC its handshake, for gRPC
	// the TCP and TLS handshakes and the HTTP/2 settings. Calls set it up
	// by themselves too; Connect is there to time the setup (EXP-6).
	Connect(ctx context.Context) error
	// Call sends payload and asks for a response of respSize bytes; it
	// returns the size of the payload received.
	Call(ctx context.Context, payload []byte, respSize int) (int, error)
	// Echo opens a stream on which the server sends back every message.
	Echo(ctx context.Context) (EchoStream, error)
	// Close closes the connection; calls in flight fail.
	Close() error
}

// EchoStream is a stream opened by Client.Echo. Send and Recv may be called
// from different goroutines.
type EchoStream interface {
	Send(seq uint64, payload []byte) error
	// Recv returns the sequence number and the payload size of the next
	// echoed message, and io.EOF once the server has answered everything
	// sent before CloseSend.
	Recv() (seq uint64, size int, err error)
	CloseSend() error
}

// Server answers the calls of the service until Stop.
type Server interface {
	Addr() net.Addr
	// Stop closes the server and its connections at once.
	Stop()
}

// Dial returns a client of transport t for the server at addr ("host:port").
// It does not connect yet.
func Dial(t Transport, addr string, tlsConf *tls.Config) (Client, error) {
	switch t {
	case QUIC:
		return dialQUIC(addr, tlsConf), nil
	case GRPCH2:
		return dialGRPC(addr, tlsConf)
	}
	return nil, fmt.Errorf("common: unknown transport %q", t)
}

// Listen starts a server of transport t on addr; port 0 picks a free one.
func Listen(t Transport, addr string, tlsConf *tls.Config) (Server, error) {
	switch t {
	case QUIC:
		return listenQUIC(addr, tlsConf)
	case GRPCH2:
		return listenGRPC(addr, tlsConf)
	}
	return nil, fmt.Errorf("common: unknown transport %q", t)
}

// StatusError is the status of a failed call, the same for every transport.
// Codes are numbered as in gRPC.
type StatusError struct {
	Code    uint32
	Message string
}

func (e *StatusError) Error() string { return fmt.Sprintf("status %d: %s", e.Code, e.Message) }

// codeUnknown is the status code of errors that carry none.
const codeUnknown = 2

// Code returns the status code of an error that a Client returned: 0 for
// nil and Unknown (2) for an error without a status.
func Code(err error) uint32 {
	if err == nil {
		return 0
	}
	if se, ok := errors.AsType[*StatusError](err); ok {
		return se.Code
	}
	return codeUnknown
}
