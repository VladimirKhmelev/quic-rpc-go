package grpcbridge

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	grpcmetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
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
	stats handlers
}

var _ grpc.ClientConnInterface = (*ClientConn)(nil)

// DialOption configures a ClientConn.
type DialOption func(*dialOptions)

type dialOptions struct {
	unary []grpc.UnaryClientInterceptor
	stats handlers
}

// WithChainUnaryInterceptor adds interceptors to unary calls. The first is
// the outermost, as with grpc.WithChainUnaryInterceptor; interceptors of
// several options run in the order of the options.
func WithChainUnaryInterceptor(interceptors ...grpc.UnaryClientInterceptor) DialOption {
	return func(o *dialOptions) { o.unary = append(o.unary, interceptors...) }
}

// WithStatsHandler adds a handler of the stats of every call, as
// grpc.WithStatsHandler does; the package documentation lists the events it
// gets. Handlers are called in the order of the options; nil is ignored.
func WithStatsHandler(h stats.Handler) DialOption {
	return func(o *dialOptions) {
		if h != nil {
			o.stats = append(o.stats, h)
		}
	}
}

// NewClient returns a client that dials with d on the first call and again
// after the connection is lost, as rpc.NewClient does.
func NewClient(d transport.Dialer, opts ...DialOption) *ClientConn {
	var o dialOptions
	for _, opt := range opts {
		opt(&o)
	}
	c := &ClientConn{cc: rpc.NewClient(d), stats: o.stats}
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

// invoke makes the call after the interceptors. As in grpc-go, the stats
// handlers tag it here, so that what they add to the outgoing metadata,
// such as the trace context of otelgrpc, is sent.
func (c *ClientConn) invoke(ctx context.Context, method string, args, reply any, _ *grpc.ClientConn, opts ...grpc.CallOption) error {
	callOpts, trailer, err := callOptions(opts)
	if err != nil {
		return err
	}
	st := c.stats.begin(ctx, method, true, false, false)
	if st != nil && trailer == nil {
		trailer = new(grpcmetadata.MD)
		callOpts = append(callOpts, rpc.Trailer((*metadata.MD)(trailer)))
	}
	err = toGRPCError(c.cc.Invoke(outgoing(st.context(ctx)), method, args, reply, callOpts...))
	if st != nil {
		st.end(err, *trailer)
	}
	return err
}

// NewStream starts a streaming call.
func (c *ClientConn) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	callOpts, _, err := callOptions(opts)
	if err != nil {
		return nil, err
	}
	st := c.stats.begin(ctx, method, true, desc.ClientStreams, desc.ServerStreams)
	d := &rpc.StreamDesc{StreamName: desc.StreamName, ServerStreams: desc.ServerStreams, ClientStreams: desc.ClientStreams}
	s, err := c.cc.NewStream(outgoing(st.context(ctx)), d, method, callOpts...)
	if err != nil {
		err = toGRPCError(err)
		st.end(err, nil)
		return nil, err
	}
	cs := &clientStream{s: s, stats: st}
	if st != nil {
		// The stream ends with its context too, as grpc-go's do.
		cs.stopWatch = context.AfterFunc(s.Context(), func() { st.end(s.Context().Err(), nil) })
	}
	return cs, nil
}

// callOptions translates the call options of grpc-go that the bridge
// supports and returns where the trailer goes, if anywhere. Header and
// Trailer store into the caller's metadata: both packages define MD as the
// same map type.
func callOptions(opts []grpc.CallOption) ([]rpc.CallOption, *grpcmetadata.MD, error) {
	var out []rpc.CallOption
	var trailer *grpcmetadata.MD
	for _, o := range opts {
		switch o := o.(type) {
		case grpc.HeaderCallOption:
			out = append(out, rpc.Header((*metadata.MD)(o.HeaderAddr)))
		case grpc.TrailerCallOption:
			out = append(out, rpc.Trailer((*metadata.MD)(o.TrailerAddr)))
			trailer = o.TrailerAddr
		case grpc.StaticMethodCallOption, grpc.EmptyCallOption:
		default:
			return nil, nil, grpcstatus.Errorf(grpccodes.Internal, "grpcbridge: call option %T is not supported", o)
		}
	}
	return out, trailer, nil
}

// clientStream is a stream of quic-rpc-go as grpc-go's generated clients
// use it.
type clientStream struct {
	s     rpc.ClientStream
	stats *call
	// stopWatch stops ending the stats with the context.
	stopWatch func() bool
}

func (c *clientStream) Header() (grpcmetadata.MD, error) {
	md, err := c.s.Header()
	return grpcmetadata.MD(md), toGRPCError(err)
}

func (c *clientStream) Trailer() grpcmetadata.MD { return grpcmetadata.MD(c.s.Trailer()) }
func (c *clientStream) CloseSend() error         { return toGRPCError(c.s.CloseSend()) }
func (c *clientStream) Context() context.Context { return c.s.Context() }
func (c *clientStream) SendMsg(m any) error      { return toGRPCError(c.s.SendMsg(m)) }

// RecvMsg receives a message; the first error ends the call, and its
// stats.
func (c *clientStream) RecvMsg(m any) error {
	err := toGRPCError(c.s.RecvMsg(m))
	if err != nil && c.stats != nil {
		c.stopWatch()
		end := err
		if errors.Is(err, io.EOF) {
			end = nil
		}
		c.stats.end(end, grpcmetadata.MD(c.s.Trailer()))
	}
	return err
}
