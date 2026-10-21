package grpcbridge_test

import (
	"context"
	"fmt"

	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/VladimirKhmelev/quic-rpc-go/grpcbridge"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/mem"
)

// Code generated for grpc-go registers its services on a grpcbridge.Server
// and calls through a grpcbridge.ClientConn. Here the transport is mem; a
// real server listens with rpcquic.NewListener and its clients dial with
// rpcquic.Dialer.
func Example() {
	l := mem.Listen(mem.Options{})
	srv := grpcbridge.NewServer()
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(l) }()
	defer srv.Stop()

	cc := grpcbridge.NewClient(l)
	defer func() { _ = cc.Close() }()
	resp, err := healthpb.NewHealthClient(cc).Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil {
		fmt.Println("Check:", err)
		return
	}
	fmt.Println(resp.GetStatus())
	// Output: SERVING
}
