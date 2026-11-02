// Package common is what every transport of the benchmarks implements
// (GEN-1): a client that makes the calls of the load generator and a server
// that answers them, both over the service of proto/bench/v1, so that one
// scenario runs on any transport unchanged.
//
// Transports: quic-rpc-go over QUIC, grpc-go over HTTP/2 with one connection
// or a pool, and connect-go's gRPC over HTTP/2 and HTTP/3. All use the TLS
// configurations they are given; the benchmarks give them the same ones
// (internal/certs).
package common

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/baseline/h2pool"
)

// Transport names a variant of the comparison.
type Transport string

// The transports.
const (
	// QUIC is quic-rpc-go over QUIC.
	QUIC Transport = "quic"
	// GRPCH2 is grpc-go over HTTP/2, one TCP connection per client.
	GRPCH2 Transport = "grpc-h2"
	// GRPCH2Pool is grpc-go over a round-robin pool of TCP connections.
	GRPCH2Pool Transport = "grpc-h2-pool"
	// GRPCH3 is connect-go's gRPC protocol over one HTTP/3 connection.
	GRPCH3 Transport = "grpc-h3"
	// ConnectH2 controls for the connect-go implementation over HTTP/2.
	ConnectH2 Transport = "connect-h2"
)

// Transports lists every transport.
var Transports = []Transport{QUIC, GRPCH2, GRPCH2Pool, GRPCH3, ConnectH2}

// ParseTransport returns the transport named s.
func ParseTransport(s string) (Transport, error) {
	for _, t := range Transports {
		if string(t) == s {
			return t, nil
		}
	}
	names := make([]string, len(Transports))
	for i, t := range Transports {
		names[i] = string(t)
	}
	return "", fmt.Errorf("transport %q: want %s", s, strings.Join(names, ", "))
}

// ClientOptions records the effective connection setup. Zero values select
// four round-robin connections for grpc-h2-pool, and one connection otherwise.
type ClientOptions struct {
	PoolSize   int    `json:"pool_size" yaml:"pool_size"`
	PoolPolicy string `json:"pool_policy" yaml:"pool_policy"`
}

// Resolve validates options and fills defaults without changing the receiver.
func (o ClientOptions) Resolve(t Transport) (ClientOptions, error) {
	if _, err := ParseTransport(string(t)); err != nil {
		return ClientOptions{}, err
	}
	size, policy := 1, "single"
	if t == GRPCH2Pool {
		size, policy = h2pool.DefaultSize, "round-robin"
	}
	if o.PoolSize == 0 {
		o.PoolSize = size
	}
	if o.PoolSize < 1 || (t != GRPCH2Pool && o.PoolSize != 1) {
		return ClientOptions{}, fmt.Errorf("%s: invalid pool_size %d", t, o.PoolSize)
	}
	if o.PoolPolicy == "" {
		o.PoolPolicy = policy
	}
	if o.PoolPolicy != policy {
		return ClientOptions{}, fmt.Errorf("%s: pool_policy must be %q", t, policy)
	}
	return o, nil
}

// Client makes calls to one server over one connection or a pool. It is safe for
// concurrent use.
type Client interface {
	// Connect waits for handshakes and, for HTTP, peer settings. For a pool
	// it waits for every member. Calls connect by themselves too;
	// Connect is there to time setup (EXP-6).
	Connect(ctx context.Context) error
	// Call sends payload and asks for a response of respSize bytes; it
	// returns the size of the payload received.
	Call(ctx context.Context, payload []byte, respSize int) (int, error)
	// Echo opens a stream on which the server sends back every message.
	Echo(ctx context.Context) (EchoStream, error)
	// Close closes all client connections; calls in flight fail.
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
// It does not connect yet. Options select the pool size; omission uses
// DefaultSize for grpc-h2-pool and one connection for other transports.
func Dial(t Transport, addr string, tlsConf *tls.Config, options ...ClientOptions) (Client, error) {
	var o ClientOptions
	if len(options) > 1 {
		return nil, errors.New("common: want at most one ClientOptions")
	}
	if len(options) == 1 {
		o = options[0]
	}
	o, err := o.Resolve(t)
	if err != nil {
		return nil, err
	}
	switch t {
	case QUIC:
		return dialQUIC(addr, tlsConf), nil
	case GRPCH2:
		return dialGRPC(addr, tlsConf)
	case GRPCH2Pool:
		return dialGRPCPool(addr, tlsConf, o.PoolSize)
	case GRPCH3, ConnectH2:
		return dialConnect(t, addr, tlsConf)
	}
	return nil, fmt.Errorf("common: unknown transport %q", t)
}

// Listen starts a server of transport t on addr; port 0 picks a free one.
func Listen(t Transport, addr string, tlsConf *tls.Config) (Server, error) {
	switch t {
	case QUIC:
		return listenQUIC(addr, tlsConf)
	case GRPCH2, GRPCH2Pool:
		return listenGRPC(addr, tlsConf)
	case GRPCH3, ConnectH2:
		return listenConnect(t, addr, tlsConf)
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
