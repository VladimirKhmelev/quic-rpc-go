package grpcbridge_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	grpcmetadata "google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/grpcbridge"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/mem"
)

// echoServer is the service of the tests, written as protoc-gen-go-grpc
// writes services.
type echoServer interface {
	Call(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)
	Repeat(*wrapperspb.StringValue, grpc.ServerStreamingServer[wrapperspb.StringValue]) error
}

// echo answers Call; Repeat sends the request back three times.
type echo func(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error)

func (f echo) Call(ctx context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
	return f(ctx, in)
}

func (echo) Repeat(in *wrapperspb.StringValue, stream grpc.ServerStreamingServer[wrapperspb.StringValue]) error {
	for range 3 {
		if err := stream.Send(in); err != nil {
			return err
		}
	}
	return nil
}

const (
	callMethod   = "/test.v1.Echo/Call"
	repeatMethod = "/test.v1.Echo/Repeat"
)

var echoDesc = grpc.ServiceDesc{
	ServiceName: "test.v1.Echo",
	HandlerType: (*echoServer)(nil),
	Methods:     []grpc.MethodDesc{{MethodName: "Call", Handler: callHandler}},
	Streams:     []grpc.StreamDesc{{StreamName: "Repeat", Handler: repeatHandler, ServerStreams: true}},
}

// repeatHandler is what protoc-gen-go-grpc generates for a server-streaming
// method.
func repeatHandler(srv any, stream grpc.ServerStream) error {
	in := new(wrapperspb.StringValue)
	if err := stream.RecvMsg(in); err != nil {
		return err
	}
	return srv.(echoServer).Repeat(in, &grpc.GenericServerStream[wrapperspb.StringValue, wrapperspb.StringValue]{ServerStream: stream}) //nolint:forcetypeassert // checked by RegisterService
}

// repeat calls Repeat as protoc-gen-go-grpc's client does.
func repeat(ctx context.Context, cc *grpcbridge.ClientConn, in string) (grpc.ServerStreamingClient[wrapperspb.StringValue], error) {
	s, err := cc.NewStream(ctx, &echoDesc.Streams[0], repeatMethod, grpc.StaticMethod())
	if err != nil {
		return nil, err
	}
	x := &grpc.GenericClientStream[wrapperspb.StringValue, wrapperspb.StringValue]{ClientStream: s}
	if err := x.SendMsg(wrapperspb.String(in)); err != nil {
		return nil, err
	}
	return x, x.CloseSend()
}

// callHandler is what protoc-gen-go-grpc generates for a unary method.
func callHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) { //nolint:revive // grpc.MethodHandler, ctx comes second
	in := new(wrapperspb.StringValue)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(echoServer).Call(ctx, in) //nolint:forcetypeassert // checked by RegisterService
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: callMethod}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(echoServer).Call(ctx, req.(*wrapperspb.StringValue)) //nolint:forcetypeassert // checked by RegisterService
	}
	return interceptor(ctx, in, info, handler)
}

// serve starts a bridge server with the echo service and health over mem
// and returns a bridge client of it.
func serve(t *testing.T, impl echo, sopts []grpcbridge.ServerOption, copts ...grpcbridge.DialOption) (*grpcbridge.ClientConn, *health.Server) {
	t.Helper()
	l := mem.Listen(mem.Options{})
	srv := grpcbridge.NewServer(sopts...)
	srv.RegisterService(&echoDesc, impl)
	hs := health.NewServer()
	healthpb.RegisterHealthServer(srv, hs)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()
	t.Cleanup(func() {
		srv.Stop()
		// Stop may come first if the test is quick.
		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("Serve: %v", err)
		}
	})
	cc := grpcbridge.NewClient(l, copts...)
	t.Cleanup(func() { _ = cc.Close() })
	return cc, hs
}

func ctx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func call(ctx context.Context, cc *grpcbridge.ClientConn, in string, opts ...grpc.CallOption) (string, error) {
	out := new(wrapperspb.StringValue)
	err := cc.Invoke(ctx, callMethod, wrapperspb.String(in), out, append([]grpc.CallOption{grpc.StaticMethod()}, opts...)...)
	return out.GetValue(), err
}

// TestMetadata: grpc-go's metadata, grpc.SetHeader, grpc.SetTrailer and
// grpc.Method work in the handler, and grpc.Header and grpc.Trailer on the
// client.
func TestMetadata(t *testing.T) {
	cc, _ := serve(t, func(ctx context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
		md, _ := grpcmetadata.FromIncomingContext(ctx)
		if m, ok := grpc.Method(ctx); !ok || m != callMethod {
			return nil, fmt.Errorf("grpc.Method = %q, %v", m, ok)
		}
		if err := grpc.SetHeader(ctx, grpcmetadata.Pairs("x-header", "h")); err != nil {
			return nil, err
		}
		if err := grpc.SetTrailer(ctx, grpcmetadata.Pairs("x-trailer", "t")); err != nil {
			return nil, err
		}
		return wrapperspb.String(in.GetValue() + " " + md.Get("x-request")[0]), nil
	}, nil)
	var header, trailer grpcmetadata.MD
	out, err := call(grpcmetadata.AppendToOutgoingContext(ctx(t), "x-request", "r1"), cc, "ping", grpc.Header(&header), grpc.Trailer(&trailer))
	if err != nil || out != "ping r1" {
		t.Fatalf("call = %q, %v", out, err)
	}
	if !slices.Equal(header.Get("x-header"), []string{"h"}) || !slices.Equal(trailer.Get("x-trailer"), []string{"t"}) {
		t.Errorf("header %v, trailer %v", header, trailer)
	}
}

// TestStatuses: errors of handlers reach the client as grpc-go would
// deliver them, details of unknown types included.
func TestStatuses(t *testing.T) {
	known, err := anypb.New(wrapperspb.String("why"))
	if err != nil {
		t.Fatal(err)
	}
	unknown := &anypb.Any{TypeUrl: "type.googleapis.com/elsewhere.v1.Reason", Value: []byte{0x0a, 0x01, 'x'}}
	wrapped := fmt.Errorf("wrapped: %w", grpcstatus.Error(grpccodes.PermissionDenied, "no"))
	tests := []struct {
		name    string
		err     error
		code    grpccodes.Code
		msg     string
		details []*anypb.Any
	}{
		{"status", grpcstatus.Error(grpccodes.NotFound, "gone"), grpccodes.NotFound, "gone", nil},
		{"details", grpcstatus.FromProto(&spb.Status{Code: int32(grpccodes.FailedPrecondition), Message: "d", Details: []*anypb.Any{known, unknown}}).Err(),
			grpccodes.FailedPrecondition, "d", []*anypb.Any{known, unknown}},
		{"wrapped", wrapped, grpccodes.PermissionDenied, wrapped.Error(), nil},
		{"code out of range", grpcstatus.Error(grpccodes.Code(20), "odd"), grpccodes.Unknown, "odd", nil},
		{"plain error", errors.New("boom"), grpccodes.Unknown, "boom", nil},
		{"context error", context.DeadlineExceeded, grpccodes.DeadlineExceeded, context.DeadlineExceeded.Error(), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cc, _ := serve(t, func(context.Context, *wrapperspb.StringValue) (*wrapperspb.StringValue, error) { return nil, tt.err }, nil)
			_, err := call(ctx(t), cc, "x")
			st, ok := grpcstatus.FromError(err)
			if !ok || st.Code() != tt.code || st.Message() != tt.msg {
				t.Fatalf("got %v (a status of grpc-go: %v), want %v %q", err, ok, tt.code, tt.msg)
			}
			details := st.Proto().GetDetails()
			if len(details) != len(tt.details) {
				t.Fatalf("details %v, want %v", details, tt.details)
			}
			for i := range details {
				if !proto.Equal(details[i], tt.details[i]) {
					t.Errorf("detail %d: %v, want %v", i, details[i], tt.details[i])
				}
			}
		})
	}
}

// TestInterceptors: both chains run in grpc-go's order, see the call and
// can stop it, as an authentication interceptor does.
func TestInterceptors(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }
	server := func(name string) grpc.UnaryServerInterceptor {
		return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			record(name + " " + info.FullMethod)
			if md, _ := grpcmetadata.FromIncomingContext(ctx); len(md.Get("x-deny")) > 0 {
				return nil, grpcstatus.Error(grpccodes.Unauthenticated, "denied by "+name)
			}
			return handler(ctx, req)
		}
	}
	client := func(name string) grpc.UnaryClientInterceptor {
		return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			record(fmt.Sprintf("%s %s %v", name, method, cc == nil))
			return invoker(grpcmetadata.AppendToOutgoingContext(ctx, "x-by", name), method, req, reply, cc, opts...)
		}
	}
	cc, _ := serve(t, func(ctx context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
		md, _ := grpcmetadata.FromIncomingContext(ctx)
		record("handler " + fmt.Sprint(md.Get("x-by")))
		return in, nil
	}, []grpcbridge.ServerOption{grpcbridge.ChainUnaryInterceptor(server("s1"), server("s2")), grpcbridge.ChainUnaryInterceptor(server("s3"))},
		grpcbridge.WithChainUnaryInterceptor(client("c1"), client("c2")), grpcbridge.WithChainUnaryInterceptor(client("c3")))

	if _, err := call(ctx(t), cc, "x"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"c1 " + callMethod + " true", "c2 " + callMethod + " true", "c3 " + callMethod + " true",
		"s1 " + callMethod, "s2 " + callMethod, "s3 " + callMethod, "handler [c1 c2 c3]",
	}
	if !slices.Equal(order, want) {
		t.Errorf("order\n got %q\nwant %q", order, want)
	}

	order = nil
	_, err := call(grpcmetadata.AppendToOutgoingContext(ctx(t), "x-deny", "1"), cc, "x")
	if st := grpcstatus.Convert(err); st.Code() != grpccodes.Unauthenticated || st.Message() != "denied by s1" || len(order) != 4 {
		t.Errorf("denied call: %v after %q", err, order)
	}
}

// TestCallOptions: options the bridge does not support fail the call
// instead of being ignored.
func TestCallOptions(t *testing.T) {
	cc, _ := serve(t, func(_ context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) { return in, nil }, nil)
	for _, opt := range []grpc.CallOption{grpc.WaitForReady(true), grpc.MaxCallRecvMsgSize(1 << 10)} {
		_, err := call(ctx(t), cc, "x", opt)
		if st := grpcstatus.Convert(err); st.Code() != grpccodes.Internal || st.Message() != fmt.Sprintf("grpcbridge: call option %T is not supported", opt) {
			t.Errorf("call with %T: %v", opt, err)
		}
		_, err = cc.NewStream(ctx(t), &healthpb.Health_ServiceDesc.Streams[0], healthpb.Health_Watch_FullMethodName, opt)
		if grpcstatus.Code(err) != grpccodes.Internal {
			t.Errorf("stream with %T: %v", opt, err)
		}
	}
}

// TestHealth runs grpc-go's own health service, its unary Check and its
// server-streaming Watch, through generated code on both sides.
func TestHealth(t *testing.T) {
	cc, hs := serve(t, nil, nil)
	hc := healthpb.NewHealthClient(cc)
	resp, err := hc.Check(ctx(t), &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("Check = %v, %v", resp, err)
	}
	if _, err := hc.Check(ctx(t), &healthpb.HealthCheckRequest{Service: "missing"}); grpcstatus.Code(err) != grpccodes.NotFound {
		t.Errorf("Check of an unknown service: %v", err)
	}

	watchCtx, cancel := context.WithCancel(ctx(t))
	w, err := hc.Watch(watchCtx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := w.Recv(); err != nil || m.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("first Watch message %v, %v", m, err)
	}
	hs.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	if m, err := w.Recv(); err != nil || m.GetStatus() != healthpb.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("second Watch message %v, %v", m, err)
	}
	cancel()
	if _, err := w.Recv(); grpcstatus.Code(err) != grpccodes.Canceled {
		t.Errorf("Recv after cancel: %v", err)
	}
}

// TestServeAfterStop: as grpc-go's, Serve after Stop returns
// grpc.ErrServerStopped.
func TestServeAfterStop(t *testing.T) {
	srv := grpcbridge.NewServer()
	srv.Stop()
	if err := srv.Serve(mem.Listen(mem.Options{})); !errors.Is(err, grpc.ErrServerStopped) {
		t.Fatalf("Serve after Stop: %v", err)
	}
}
