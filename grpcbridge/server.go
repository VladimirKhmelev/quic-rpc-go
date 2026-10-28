package grpcbridge

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc"
	grpcmetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// Server is a server of quic-rpc-go for services that code generated for
// grpc-go registers.
type Server struct {
	s           *rpc.Server
	unary       grpc.UnaryServerInterceptor
	stats       handlers
	stopTimeout time.Duration
}

var _ grpc.ServiceRegistrar = (*Server)(nil)

// ServerOption configures a Server.
type ServerOption func(*serverOptions)

type serverOptions struct {
	unary       []grpc.UnaryServerInterceptor
	stats       handlers
	stopTimeout time.Duration
}

// DefaultGracefulStopTimeout is how long GracefulStop waits by default:
// half of the 10 s that docker waits after SIGTERM before it kills the
// process, which also stops its gRPC server meanwhile.
const DefaultGracefulStopTimeout = 5 * time.Second

// GracefulStopTimeout sets how long GracefulStop waits for the calls in
// flight before it stops the server at once.
func GracefulStopTimeout(d time.Duration) ServerOption {
	return func(o *serverOptions) { o.stopTimeout = d }
}

// ChainUnaryInterceptor adds interceptors to unary calls. The first is the
// outermost, as with grpc.ChainUnaryInterceptor; interceptors of several
// options run in the order of the options.
func ChainUnaryInterceptor(interceptors ...grpc.UnaryServerInterceptor) ServerOption {
	return func(o *serverOptions) { o.unary = append(o.unary, interceptors...) }
}

// StatsHandler adds a handler of the stats of every call, as
// grpc.StatsHandler does; the package documentation lists the events it
// gets. Handlers are called in the order of the options; nil is ignored.
func StatsHandler(h stats.Handler) ServerOption {
	return func(o *serverOptions) {
		if h != nil {
			o.stats = append(o.stats, h)
		}
	}
}

// NewServer returns a server with no services registered.
func NewServer(opts ...ServerOption) *Server {
	o := serverOptions{stopTimeout: DefaultGracefulStopTimeout}
	for _, opt := range opts {
		opt(&o)
	}
	return &Server{s: rpc.NewServer(), unary: chainUnaryServer(o.unary), stats: o.stats, stopTimeout: o.stopTimeout}
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
				// As in grpc-go, the stats handlers tag the call before the
				// interceptors run, with its incoming metadata.
				ctx = handlerContext(ctx, method, nil)
				st := s.stats.begin(ctx, method, false, false, false)
				resp, err := handler(srv, st.context(ctx), decode, s.unary)
				st.end(err, nil)
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
				ctx := handlerContext(stream.Context(), method, stream)
				st := s.stats.begin(ctx, method, false, sd.ClientStreams, sd.ServerStreams)
				err := handler(srv, &serverStream{s: stream, ctx: st.context(ctx)})
				st.end(err, nil)
				return toRPCError(err)
			},
		})
	}
	s.s.RegisterService(d, impl)
}

// Serve accepts connections on l and serves them until Stop or
// GracefulStop, and returns nil then; see rpc.Server.Serve. Called after
// them, it returns grpc.ErrServerStopped, as grpc-go does.
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

// GracefulStop stops the server gently, as rpc.Server.GracefulStop does: it
// takes no more connections and sends GOAWAY, so the clients send new calls
// elsewhere, lets the calls in flight finish, and waits for the clients to
// close their connections, at most the timeout of GracefulStopTimeout.
func (s *Server) GracefulStop() {
	ctx, cancel := context.WithTimeout(context.Background(), s.stopTimeout)
	defer cancel()
	s.s.GracefulStop(ctx)
}

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
