package grpcbridge

import (
	"context"

	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	grpcmetadata "google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// ClientConn is a client of quic-rpc-go for code generated for grpc-go. It
// is safe for concurrent use.
type ClientConn struct {
	cc    *rpc.ClientConn
	unary grpc.UnaryInvoker
}

var _ grpc.ClientConnInterface = (*ClientConn)(nil)

// DialOption configures a ClientConn.
type DialOption func(*dialOptions)

type dialOptions struct {
	unary []grpc.UnaryClientInterceptor
}

// WithChainUnaryInterceptor adds interceptors to unary calls. The first is
// the outermost, as with grpc.WithChainUnaryInterceptor; interceptors of
// several options run in the order of the options.
func WithChainUnaryInterceptor(interceptors ...grpc.UnaryClientInterceptor) DialOption {
	return func(o *dialOptions) { o.unary = append(o.unary, interceptors...) }
}

// NewClient returns a client that dials with d on the first call and again
// after the connection is lost, as rpc.NewClient does.
func NewClient(d transport.Dialer, opts ...DialOption) *ClientConn {
	var o dialOptions
	for _, opt := range opts {
		opt(&o)
	}
	c := &ClientConn{cc: rpc.NewClient(d)}
	c.unary = c.invoke
	for i := len(o.unary) - 1; i >= 0; i-- {
		interceptor, next := o.unary[i], c.unary
		c.unary = func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, opts ...grpc.CallOption) error {
			return interceptor(ctx, method, req, reply, cc, next, opts...)
		}
	}
	return c
}

// Close closes the connection; calls in flight fail with Canceled.
func (c *ClientConn) Close() error { return c.cc.Close() }

// Invoke performs a unary call through the interceptors.
func (c *ClientConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	return c.unary(ctx, method, args, reply, nil, opts...)
}

func (c *ClientConn) invoke(ctx context.Context, method string, args, reply any, _ *grpc.ClientConn, opts ...grpc.CallOption) error {
	callOpts, err := callOptions(opts)
	if err != nil {
		return err
	}
	return toGRPCError(c.cc.Invoke(outgoing(ctx), method, args, reply, callOpts...))
}

// NewStream starts a streaming call.
func (c *ClientConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	callOpts, err := callOptions(opts)
	if err != nil {
		return nil, err
	}
	d := &rpc.StreamDesc{StreamName: desc.StreamName, ServerStreams: desc.ServerStreams, ClientStreams: desc.ClientStreams}
	s, err := c.cc.NewStream(outgoing(ctx), d, method, callOpts...)
	if err != nil {
		return nil, toGRPCError(err)
	}
	return clientStream{s}, nil
}

// callOptions translates the call options of grpc-go that the bridge
// supports. Header and Trailer store into the caller's metadata: both
// packages define MD as the same map type.
func callOptions(opts []grpc.CallOption) ([]rpc.CallOption, error) {
	var out []rpc.CallOption
	for _, o := range opts {
		switch o := o.(type) {
		case grpc.HeaderCallOption:
			out = append(out, rpc.Header((*metadata.MD)(o.HeaderAddr)))
		case grpc.TrailerCallOption:
			out = append(out, rpc.Trailer((*metadata.MD)(o.TrailerAddr)))
		case grpc.StaticMethodCallOption, grpc.EmptyCallOption:
		default:
			return nil, grpcstatus.Errorf(grpccodes.Internal, "grpcbridge: call option %T is not supported", o)
		}
	}
	return out, nil
}

// clientStream is a stream of quic-rpc-go as grpc-go's generated clients
// use it.
type clientStream struct {
	s rpc.ClientStream
}

func (c clientStream) Header() (grpcmetadata.MD, error) {
	md, err := c.s.Header()
	return grpcmetadata.MD(md), toGRPCError(err)
}

func (c clientStream) Trailer() grpcmetadata.MD { return grpcmetadata.MD(c.s.Trailer()) }
func (c clientStream) CloseSend() error         { return toGRPCError(c.s.CloseSend()) }
func (c clientStream) Context() context.Context { return c.s.Context() }
func (c clientStream) SendMsg(m any) error      { return toGRPCError(c.s.SendMsg(m)) }
func (c clientStream) RecvMsg(m any) error      { return toGRPCError(c.s.RecvMsg(m)) }
