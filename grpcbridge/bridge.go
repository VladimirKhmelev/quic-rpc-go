// Package grpcbridge runs code generated for grpc-go over quic-rpc-go.
// ClientConn implements grpc.ClientConnInterface and Server implements
// grpc.ServiceRegistrar, so the clients and the registration functions that
// protoc-gen-go-grpc generates work over QUIC unchanged, and so does the
// code around them that uses grpc-go's packages: metadata, status,
// grpc.SetHeader, grpc.SendHeader, grpc.SetTrailer, grpc.Method and unary
// interceptors.
//
// The bridge translates:
//   - metadata: the outgoing metadata of grpc-go on the client, the
//     incoming metadata of grpc-go on the server;
//   - statuses, both ways, with their details as they are, including
//     details of types the program does not know. A code above 16, which
//     quic-rpc-go cannot send, becomes Unknown;
//   - the call options grpc.Header, grpc.Trailer and grpc.StaticMethod,
//     which generated code adds to every call. Any other call option fails
//     the call with Internal rather than being ignored;
//   - unary interceptors, chained as grpc.ChainUnaryInterceptor and
//     grpc.WithChainUnaryInterceptor chain them. Client interceptors get a
//     nil *grpc.ClientConn.
//
// It does not provide the peer of a call (peer.FromContext finds nothing),
// stream interceptors or compression.
package grpcbridge

import (
	"context"
	"errors"
	"io"

	spb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	grpcmetadata "google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

// toGRPCError turns an error of quic-rpc-go into the error grpc-go would
// return: a status error with the same code, message and details. io.EOF
// and errors without a status pass unchanged.
func toGRPCError(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	c, msg, details := status.Wire(st)
	// FromProto copies the details, which Wire does not let us modify.
	return grpcstatus.FromProto(&spb.Status{Code: grpcCode(c), Message: msg, Details: details}).Err()
}

// toRPCError turns an error that grpc-go code returned into one that
// quic-rpc-go sends: a status error of grpc-go becomes the same status.
// Other errors pass unchanged, and quic-rpc-go makes them Unknown, or
// Canceled and DeadlineExceeded for context errors, as grpc-go does.
func toRPCError(err error) error {
	if err == nil {
		return nil
	}
	st, ok := grpcstatus.FromError(err)
	if !ok {
		return err
	}
	// Proto returns a copy, which the status may own.
	p := st.Proto()
	return status.FromWire(rpcCode(p.GetCode()), p.GetMessage(), p.GetDetails()).Err()
}

// grpcCode returns c as grpc-go keeps codes; both number them as gRPC does.
func grpcCode(c codes.Code) int32 {
	if c > codes.Unauthenticated {
		return int32(codes.Unknown)
	}
	return int32(c) //nolint:gosec // at most 16
}

// rpcCode returns c as quic-rpc-go keeps codes, Unknown for a code outside
// gRPC's 0 to 16, which quic-rpc-go cannot send.
func rpcCode(c int32) codes.Code {
	if c < 0 || c > int32(codes.Unauthenticated) {
		return codes.Unknown
	}
	return codes.Code(c) //nolint:gosec // between 0 and 16
}

// outgoing puts the outgoing metadata of grpc-go into ctx for quic-rpc-go,
// together with any that ctx already has for quic-rpc-go.
func outgoing(ctx context.Context) context.Context {
	md, ok := grpcmetadata.FromOutgoingContext(ctx)
	if !ok {
		return ctx
	}
	if own, ok := metadata.FromOutgoingContext(ctx); ok {
		return metadata.NewOutgoingContext(ctx, metadata.Join(own, metadata.MD(md)))
	}
	return metadata.NewOutgoingContext(ctx, metadata.MD(md))
}

// handlerContext is the context grpc-go would give the handler of method:
// it carries the incoming metadata of grpc-go and a transport stream for
// grpc.SetHeader and its siblings. stream is nil for a unary call.
func handlerContext(ctx context.Context, method string, stream rpc.ServerStream) context.Context {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		ctx = grpcmetadata.NewIncomingContext(ctx, grpcmetadata.MD(md))
	}
	return grpc.NewContextWithServerTransportStream(ctx, &transportStream{ctx: ctx, method: method, stream: stream})
}

// transportStream is what grpc.SetHeader, grpc.SendHeader, grpc.SetTrailer
// and grpc.Method find in the context of a handler.
type transportStream struct {
	ctx    context.Context
	method string
	stream rpc.ServerStream
}

func (s *transportStream) Method() string { return s.method }

func (s *transportStream) SetHeader(md grpcmetadata.MD) error {
	if s.stream != nil {
		return toGRPCError(s.stream.SetHeader(metadata.MD(md)))
	}
	return toGRPCError(rpc.SetHeader(s.ctx, metadata.MD(md)))
}

func (s *transportStream) SendHeader(md grpcmetadata.MD) error {
	if s.stream != nil {
		return toGRPCError(s.stream.SendHeader(metadata.MD(md)))
	}
	return toGRPCError(rpc.SendHeader(s.ctx, metadata.MD(md)))
}

func (s *transportStream) SetTrailer(md grpcmetadata.MD) error {
	if s.stream != nil {
		s.stream.SetTrailer(metadata.MD(md))
		return nil
	}
	return toGRPCError(rpc.SetTrailer(s.ctx, metadata.MD(md)))
}
