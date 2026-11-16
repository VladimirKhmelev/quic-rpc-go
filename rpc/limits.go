package rpc

import (
	"google.golang.org/protobuf/proto"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

// MaxRecvMsgSize sets the server's maximum serialized protobuf message size
// in bytes. The default is 4 MiB. Zero permits only empty messages; a negative
// size panics. Headers and trailers have separate, fixed limits.
func MaxRecvMsgSize(n int) ServerOption {
	checkServerMessageSize(n)
	return func(o *serverOptions) { o.maxRecv = n }
}

// MaxSendMsgSize sets the server's maximum serialized protobuf message size
// in bytes. Sending is unlimited by default. Zero permits only empty messages;
// a negative size panics.
func MaxSendMsgSize(n int) ServerOption {
	checkServerMessageSize(n)
	return func(o *serverOptions) { o.maxSend = n }
}

func checkServerMessageSize(n int) {
	if n < 0 {
		panic("rpc: message size must be nonnegative")
	}
}

// WithDefaultCallOptions appends options applied before the options of each
// call, including before client interceptors. Later options override earlier
// ones. The caller may reuse the supplied slice after this function returns.
func WithDefaultCallOptions(opts ...CallOption) DialOption {
	opts = append([]CallOption(nil), opts...)
	return func(o *dialOptions) { o.defaults = append(o.defaults, opts...) }
}

// MaxCallRecvMsgSize sets the maximum serialized protobuf message size the
// client accepts, in bytes. The default is 4 MiB. Zero permits only empty
// messages; a negative size fails the call with InvalidArgument.
func MaxCallRecvMsgSize(n int) CallOption {
	return callOptionFunc(func(ci *callInfo) { ci.maxRecv = n })
}

// MaxCallSendMsgSize sets the maximum serialized protobuf message size the
// client sends, in bytes. Sending is unlimited by default. Zero permits only
// empty messages; a negative size fails the call with InvalidArgument.
func MaxCallSendMsgSize(n int) CallOption {
	return callOptionFunc(func(ci *callInfo) {
		ci.maxSend = n
		ci.sendLimitSet = true
	})
}

func recvLimits(n int) frame.Limits {
	l := frame.DefaultLimits()
	l.MaxMessageSize = uint64(n) //nolint:gosec // validated nonnegative before opening a stream
	return l
}

// checkSendSize runs before marshaling or growing a message buffer. A negative
// limit is the internal sentinel for the unchanged, unlimited send default.
func checkSendSize(m any, limit int) error {
	if limit < 0 {
		return nil
	}
	pm, ok := m.(proto.Message)
	if !ok {
		return status.Errorf(codes.Internal, "message of type %T is not a protobuf message", m)
	}
	if n := proto.Size(pm); n > limit {
		return status.Errorf(codes.ResourceExhausted, "sent message larger than max (%d vs. %d)", n, limit)
	}
	return nil
}
