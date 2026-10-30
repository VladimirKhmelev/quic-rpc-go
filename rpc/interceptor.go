package rpc

import "context"

// Interceptors wrap calls as in grpc-go: the same signatures, with this
// package's types. A Server takes chains of them through ChainUnaryInterceptor
// and ChainStreamInterceptor, a ClientConn through WithChainUnaryInterceptor
// and WithChainStreamInterceptor. The first interceptor of a chain is the
// outermost. Unlike grpc-go, there are no options that set a single
// interceptor: a chain of one does that.

// StreamServerInfo describes a streaming call to interceptors.
type StreamServerInfo struct {
	// FullMethod is "/package.Service/Method".
	FullMethod string
	// IsClientStream and IsServerStream report whether the client and the
	// server may send several messages.
	IsClientStream bool
	IsServerStream bool
}

// StreamServerInterceptor wraps streaming calls on the server. It runs the
// method by calling handler, with ss or a stream that wraps it.
type StreamServerInterceptor func(srv any, ss ServerStream, info *StreamServerInfo, handler StreamHandler) error

// UnaryInvoker performs a unary call; the innermost client interceptor
// calls it.
type UnaryInvoker func(ctx context.Context, method string, req, reply any, cc *ClientConn, opts ...CallOption) error

// UnaryClientInterceptor wraps unary calls on the client. It performs the
// call by calling invoker.
type UnaryClientInterceptor func(ctx context.Context, method string, req, reply any, cc *ClientConn, invoker UnaryInvoker, opts ...CallOption) error

// Streamer starts a streaming call; the innermost client interceptor calls
// it.
type Streamer func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, opts ...CallOption) (ClientStream, error)

// StreamClientInterceptor wraps streaming calls on the client. It starts
// the call by calling streamer and may wrap the stream it returns.
type StreamClientInterceptor func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, streamer Streamer, opts ...CallOption) (ClientStream, error)

// ServerOption configures a Server.
type ServerOption func(*serverOptions)

type serverOptions struct {
	unary  []UnaryServerInterceptor
	stream []StreamServerInterceptor
}

// ChainUnaryInterceptor adds interceptors to unary calls. Interceptors of
// several options run in the order of the options.
func ChainUnaryInterceptor(interceptors ...UnaryServerInterceptor) ServerOption {
	return func(o *serverOptions) { o.unary = append(o.unary, interceptors...) }
}

// ChainStreamInterceptor adds interceptors to streaming calls. Interceptors
// of several options run in the order of the options.
func ChainStreamInterceptor(interceptors ...StreamServerInterceptor) ServerOption {
	return func(o *serverOptions) { o.stream = append(o.stream, interceptors...) }
}

// DialOption configures a ClientConn.
type DialOption func(*dialOptions)

type dialOptions struct {
	unary  []UnaryClientInterceptor
	stream []StreamClientInterceptor
}

// WithChainUnaryInterceptor adds interceptors to unary calls. Interceptors
// of several options run in the order of the options.
func WithChainUnaryInterceptor(interceptors ...UnaryClientInterceptor) DialOption {
	return func(o *dialOptions) { o.unary = append(o.unary, interceptors...) }
}

// WithChainStreamInterceptor adds interceptors to streaming calls.
// Interceptors of several options run in the order of the options.
func WithChainStreamInterceptor(interceptors ...StreamClientInterceptor) DialOption {
	return func(o *dialOptions) { o.stream = append(o.stream, interceptors...) }
}

// The chain functions make one interceptor of several, the first
// outermost, as grpc-go's chainUnaryServerInterceptors and its kin do
// (server.go, clientconn.go); they return nil for no interceptors.

func chainUnaryServer(interceptors []UnaryServerInterceptor) UnaryServerInterceptor {
	if len(interceptors) <= 1 {
		return first(interceptors)
	}
	return func(ctx context.Context, req any, info *UnaryServerInfo, handler UnaryHandler) (any, error) {
		return interceptors[0](ctx, req, info, unaryServerNext(interceptors[1:], info, handler))
	}
}

func unaryServerNext(rest []UnaryServerInterceptor, info *UnaryServerInfo, handler UnaryHandler) UnaryHandler {
	if len(rest) == 0 {
		return handler
	}
	return func(ctx context.Context, req any) (any, error) {
		return rest[0](ctx, req, info, unaryServerNext(rest[1:], info, handler))
	}
}

func chainStreamServer(interceptors []StreamServerInterceptor) StreamServerInterceptor {
	if len(interceptors) <= 1 {
		return first(interceptors)
	}
	return func(srv any, ss ServerStream, info *StreamServerInfo, handler StreamHandler) error {
		return interceptors[0](srv, ss, info, streamServerNext(interceptors[1:], info, handler))
	}
}

func streamServerNext(rest []StreamServerInterceptor, info *StreamServerInfo, handler StreamHandler) StreamHandler {
	if len(rest) == 0 {
		return handler
	}
	return func(srv any, ss ServerStream) error {
		return rest[0](srv, ss, info, streamServerNext(rest[1:], info, handler))
	}
}

func chainUnaryClient(interceptors []UnaryClientInterceptor) UnaryClientInterceptor {
	if len(interceptors) <= 1 {
		return first(interceptors)
	}
	return func(ctx context.Context, method string, req, reply any, cc *ClientConn, invoker UnaryInvoker, opts ...CallOption) error {
		return interceptors[0](ctx, method, req, reply, cc, unaryClientNext(interceptors[1:], invoker), opts...)
	}
}

func unaryClientNext(rest []UnaryClientInterceptor, invoker UnaryInvoker) UnaryInvoker {
	if len(rest) == 0 {
		return invoker
	}
	return func(ctx context.Context, method string, req, reply any, cc *ClientConn, opts ...CallOption) error {
		return rest[0](ctx, method, req, reply, cc, unaryClientNext(rest[1:], invoker), opts...)
	}
}

func chainStreamClient(interceptors []StreamClientInterceptor) StreamClientInterceptor {
	if len(interceptors) <= 1 {
		return first(interceptors)
	}
	return func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, streamer Streamer, opts ...CallOption) (ClientStream, error) {
		return interceptors[0](ctx, desc, cc, method, streamClientNext(interceptors[1:], streamer), opts...)
	}
}

func streamClientNext(rest []StreamClientInterceptor, streamer Streamer) Streamer {
	if len(rest) == 0 {
		return streamer
	}
	return func(ctx context.Context, desc *StreamDesc, cc *ClientConn, method string, opts ...CallOption) (ClientStream, error) {
		return rest[0](ctx, desc, cc, method, streamClientNext(rest[1:], streamer), opts...)
	}
}

// first returns the only interceptor of a chain, or nil for none.
func first[T any](interceptors []T) T {
	var zero T
	if len(interceptors) == 0 {
		return zero
	}
	return interceptors[0]
}
