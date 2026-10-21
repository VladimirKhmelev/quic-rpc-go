package grpcbridge

import (
	"context"
	"errors"

	"google.golang.org/grpc"
	grpcmetadata "google.golang.org/grpc/metadata"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// Server is a server of quic-rpc-go for services that code generated for
// grpc-go registers.
type Server struct {
	s     *rpc.Server
	unary grpc.UnaryServerInterceptor
}

var _ grpc.ServiceRegistrar = (*Server)(nil)

// ServerOption configures a Server.
type ServerOption func(*serverOptions)

type serverOptions struct {
	unary []grpc.UnaryServerInterceptor
}

// ChainUnaryInterceptor adds interceptors to unary calls. The first is the
// outermost, as with grpc.ChainUnaryInterceptor; interceptors of several
// options run in the order of the options.
func ChainUnaryInterceptor(interceptors ...grpc.UnaryServerInterceptor) ServerOption {
	return func(o *serverOptions) { o.unary = append(o.unary, interceptors...) }
}

// NewServer returns a server with no services registered.
func NewServer(opts ...ServerOption) *Server {
	var o serverOptions
	for _, opt := range opts {
		opt(&o)
	}
	return &Server{s: rpc.NewServer(), unary: chainUnaryServer(o.unary)}
}

// chainUnaryServer makes one interceptor of several, the first outermost;
// generated handlers take a single one.
func chainUnaryServer(interceptors []grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	switch len(interceptors) {
	case 0:
		return nil
	case 1:
		return interceptors[0]
	}
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		for i := len(interceptors) - 1; i > 0; i-- {
			interceptor, next := interceptors[i], handler
			handler = func(ctx context.Context, req any) (any, error) { return interceptor(ctx, req, info, next) }
		}
		return interceptors[0](ctx, req, info, handler)
	}
}

// RegisterService registers a service and its implementation, as
// grpc.Server.RegisterService does; it panics on the same mistakes.
func (s *Server) RegisterService(desc *grpc.ServiceDesc, impl any) {
	d := &rpc.ServiceDesc{ServiceName: desc.ServiceName, HandlerType: desc.HandlerType, Metadata: desc.Metadata}
	for _, m := range desc.Methods {
		handler, method := m.Handler, "/"+desc.ServiceName+"/"+m.MethodName
		d.Methods = append(d.Methods, rpc.MethodDesc{
			MethodName: m.MethodName,
			Handler: func(srv any, ctx context.Context, dec func(any) error, _ rpc.UnaryServerInterceptor) (any, error) { //nolint:revive // rpc.MethodHandler mirrors grpc-go, ctx comes second
				decode := func(m any) error { return toGRPCError(dec(m)) }
				resp, err := handler(srv, handlerContext(ctx, method, nil), decode, s.unary)
				return resp, toRPCError(err)
			},
		})
	}
	for _, sd := range desc.Streams {
		handler, method := sd.Handler, "/"+desc.ServiceName+"/"+sd.StreamName
		d.Streams = append(d.Streams, rpc.StreamDesc{
			StreamName:    sd.StreamName,
			ServerStreams: sd.ServerStreams,
			ClientStreams: sd.ClientStreams,
			Handler: func(srv any, stream rpc.ServerStream) error {
				return toRPCError(handler(srv, &serverStream{s: stream, ctx: handlerContext(stream.Context(), method, stream)}))
			},
		})
	}
	s.s.RegisterService(d, impl)
}

// Serve accepts connections on l and serves them until Stop; see
// rpc.Server.Serve. Called after Stop, it returns grpc.ErrServerStopped, as
// grpc-go does.
func (s *Server) Serve(l transport.Listener) error {
	err := s.s.Serve(l)
	if errors.Is(err, rpc.ErrServerStopped) {
		return grpc.ErrServerStopped
	}
	return err
}

// Stop closes the listeners and the connections at once; calls in flight
// fail with Unavailable on the client.
func (s *Server) Stop() { s.s.Stop() }

// serverStream is a stream of quic-rpc-go as grpc-go's generated handlers
// use it.
type serverStream struct {
	s   rpc.ServerStream
	ctx context.Context
}

func (s *serverStream) SetHeader(md grpcmetadata.MD) error {
	return toGRPCError(s.s.SetHeader(metadata.MD(md)))
}

func (s *serverStream) SendHeader(md grpcmetadata.MD) error {
	return toGRPCError(s.s.SendHeader(metadata.MD(md)))
}

func (s *serverStream) SetTrailer(md grpcmetadata.MD) { s.s.SetTrailer(metadata.MD(md)) }
func (s *serverStream) Context() context.Context      { return s.ctx }
func (s *serverStream) SendMsg(m any) error           { return toGRPCError(s.s.SendMsg(m)) }
func (s *serverStream) RecvMsg(m any) error           { return toGRPCError(s.s.RecvMsg(m)) }
