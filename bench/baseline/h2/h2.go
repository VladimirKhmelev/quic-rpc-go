// Package h2 is the gRPC/HTTP/2 baseline: grpc-go over TLS with one TCP
// connection per client.
//
// It keeps grpc-go's defaults (v1.84.0), which the comparison with
// quic-rpc-go has to state:
//   - flow control: 64 KiB initial stream and connection windows, grown by
//     BDP estimation (internal/transport/defaults.go, http2_client.go);
//   - messages: up to 4 MiB received, unlimited sent (server.go,
//     clientconn.go);
//   - keepalive: no client pings; the server pings after 2 hours of
//     inactivity and allows client pings at most every 5 minutes;
//   - TLS: the given config; grpc-go adds the ALPN "h2".
package h2

import (
	"crypto/tls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// NewServer returns a gRPC server that serves TLS with tlsConf.
func NewServer(tlsConf *tls.Config, opts ...grpc.ServerOption) *grpc.Server {
	return grpc.NewServer(append([]grpc.ServerOption{grpc.Creds(credentials.NewTLS(tlsConf))}, opts...)...)
}

// Dial returns a client connection to the server at addr ("host:port"),
// verified with tlsConf. Like grpc.NewClient it connects on the first call.
// The address is used as is, without name resolution.
func Dial(addr string, tlsConf *tls.Config, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///"+addr,
		append([]grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsConf))}, opts...)...)
}
