package grpcbridge

import (
	"google.golang.org/grpc"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
)

// MaxRecvMsgSize sets the server's maximum serialized protobuf message size
// in bytes. The default is 4 MiB. Zero permits only empty messages; a negative
// size panics.
func MaxRecvMsgSize(n int) ServerOption {
	opt := rpc.MaxRecvMsgSize(n)
	return func(o *serverOptions) { o.rpc = append(o.rpc, opt) }
}

// MaxSendMsgSize sets the server's maximum serialized protobuf message size
// in bytes. Sending is unlimited by default. Zero permits only empty messages;
// a negative size panics.
func MaxSendMsgSize(n int) ServerOption {
	opt := rpc.MaxSendMsgSize(n)
	return func(o *serverOptions) { o.rpc = append(o.rpc, opt) }
}

// WithDefaultCallOptions appends grpc-go call options applied before each
// call's options and before client interceptors. The bridge supports message
// size limits as well as the options listed in its package documentation.
func WithDefaultCallOptions(opts ...grpc.CallOption) DialOption {
	opts = append([]grpc.CallOption(nil), opts...)
	return func(o *dialOptions) { o.defaults = append(o.defaults, opts...) }
}
