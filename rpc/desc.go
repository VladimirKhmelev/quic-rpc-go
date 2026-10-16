// Package rpc is an RPC layer on top of native QUIC streams (quic-go): one
// call maps to one bidirectional stream, with no HTTP in between.
//
// The API mirrors grpc-go: services are described by ServiceDesc, clients
// call ClientConn.Invoke and ClientConn.NewStream, servers register
// implementations with Server.RegisterService. Statuses, codes and metadata
// live in the status, codes and metadata subpackages. Messages are protobuf
// messages; the wire format is described in docs/protocol.md.
package rpc

import "context"

// MethodHandler runs a unary method of srv. dec decodes the request into its
// argument; interceptor, when not nil, wraps the call to the implementation.
// Generated code provides the handlers.
type MethodHandler func(srv any, ctx context.Context, dec func(any) error, interceptor UnaryServerInterceptor) (any, error)

// MethodDesc describes a unary method.
type MethodDesc struct {
	MethodName string
	Handler    MethodHandler
}

// StreamHandler runs a streaming method of srv.
type StreamHandler func(srv any, stream ServerStream) error

// StreamDesc describes a streaming method.
type StreamDesc struct {
	StreamName string
	Handler    StreamHandler
	// ServerStreams reports whether the server may send several messages.
	ServerStreams bool
	// ClientStreams reports whether the client may send several messages.
	ClientStreams bool
}

// ServiceDesc describes a service.
type ServiceDesc struct {
	ServiceName string
	// HandlerType is a pointer to the service interface; RegisterService
	// checks that the implementation satisfies it.
	HandlerType any
	Methods     []MethodDesc
	Streams     []StreamDesc
	// Metadata is the name of the .proto file the service comes from.
	Metadata any
}

// UnaryServerInfo describes a unary call to interceptors.
type UnaryServerInfo struct {
	// Server is the service implementation.
	Server any
	// FullMethod is "/package.Service/Method".
	FullMethod string
}

// UnaryHandler calls the implementation of a unary method.
type UnaryHandler func(ctx context.Context, req any) (any, error)

// UnaryServerInterceptor wraps unary calls on the server. Chains of
// interceptors are configured on the Server in phase 3 of the project.
type UnaryServerInterceptor func(ctx context.Context, req any, info *UnaryServerInfo, handler UnaryHandler) (resp any, err error)

// ClientConnInterface is what generated clients call: *ClientConn implements it.
type ClientConnInterface interface {
	// Invoke performs a unary call.
	Invoke(ctx context.Context, method string, args, reply any, opts ...CallOption) error
	// NewStream starts a streaming call.
	NewStream(ctx context.Context, desc *StreamDesc, method string, opts ...CallOption) (ClientStream, error)
}

// ServiceRegistrar is what generated registration functions call: *Server
// implements it.
type ServiceRegistrar interface {
	RegisterService(desc *ServiceDesc, impl any)
}
