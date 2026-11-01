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
//
// One default it does not keep: grpc-go dials through the proxy that
// HTTPS_PROXY names, and the calls of a benchmark must go straight to the
// server.
package h2

import (
	"context"
	"crypto/tls"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// NewServer returns a gRPC server that serves TLS with tlsConf.
func NewServer(tlsConf *tls.Config, opts ...grpc.ServerOption) *grpc.Server {
	return grpc.NewServer(append([]grpc.ServerOption{grpc.Creds(credentials.NewTLS(tlsConf))}, opts...)...)
}

// Connect starts dialing and waits for TCP, TLS and HTTP/2 settings. As with
// ordinary RPCs, an unavailable connection fails without waiting for recovery.
func Connect(ctx context.Context, cc *grpc.ClientConn) error {
	if err := ctx.Err(); err != nil {
		return status.FromContextError(err).Err()
	}
	cc.Connect()
	for {
		if err := ctx.Err(); err != nil {
			return status.FromContextError(err).Err()
		}
		switch s := cc.GetState(); s {
		case connectivity.Ready:
			return nil
		case connectivity.TransientFailure, connectivity.Shutdown:
			return status.Error(codes.Unavailable, "connection "+s.String())
		default:
			if !cc.WaitForStateChange(ctx, s) {
				return status.FromContextError(ctx.Err()).Err()
			}
		}
	}
}

// Dial returns a client connection to the server at addr ("host:port"),
// verified with tlsConf. Like grpc.NewClient it connects on the first call.
// The address is used as is, without name resolution or a proxy.
func Dial(addr string, tlsConf *tls.Config, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return grpc.NewClient("passthrough:///"+addr,
		append([]grpc.DialOption{grpc.WithTransportCredentials(credentials.NewTLS(tlsConf)), grpc.WithNoProxy()}, opts...)...)
}
