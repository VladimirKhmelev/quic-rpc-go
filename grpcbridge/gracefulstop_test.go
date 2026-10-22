package grpcbridge_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/grpcbridge"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/mem"
)

// TestGracefulStop: the call in flight finishes and GracefulStop returns
// with it; Serve returns nil, which the services of the messenger rely on:
// they exit on any error from Serve.
func TestGracefulStop(t *testing.T) {
	for _, tt := range []struct {
		name    string
		opts    []grpcbridge.ServerOption
		handler time.Duration
		want    time.Duration
		code    grpccodes.Code
	}{
		{"call finishes", nil, time.Second, time.Second, grpccodes.OK},
		{"default timeout", nil, time.Minute, grpcbridge.DefaultGracefulStopTimeout, grpccodes.Unavailable},
		{"timeout option", []grpcbridge.ServerOption{grpcbridge.GracefulStopTimeout(2 * time.Second)}, time.Minute, 2 * time.Second, grpccodes.Unavailable},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				l := mem.Listen(mem.Options{})
				srv := grpcbridge.NewServer(tt.opts...)
				srv.RegisterService(&echoDesc, echo(func(ctx context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
					select {
					case <-time.After(tt.handler):
						return in, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}))
				served := make(chan error, 1)
				go func() { served <- srv.Serve(l) }()
				cc := grpcbridge.NewClient(l)
				defer func() { _ = cc.Close() }()

				result := make(chan error, 1)
				go func() { _, err := call(t.Context(), cc, "x"); result <- err }()
				synctest.Wait()
				start := time.Now()
				srv.GracefulStop()
				if d := time.Since(start); d != tt.want {
					t.Errorf("GracefulStop took %v, want %v", d, tt.want)
				}
				if err := <-served; err != nil {
					t.Errorf("Serve after GracefulStop: %v", err)
				}
				if err := <-result; grpcstatus.Code(err) != tt.code {
					t.Errorf("call in flight: %v, want %v", err, tt.code)
				}
			})
		})
	}
}
