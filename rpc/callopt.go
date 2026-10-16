package rpc

import "github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"

// CallOption configures a single call.
type CallOption interface {
	apply(*callInfo)
}

type callInfo struct {
	header  *metadata.MD
	trailer *metadata.MD
}

type callOptionFunc func(*callInfo)

func (f callOptionFunc) apply(ci *callInfo) { f(ci) }

// Header stores the server's header metadata into *md when the call ends.
func Header(md *metadata.MD) CallOption {
	return callOptionFunc(func(ci *callInfo) { ci.header = md })
}

// Trailer stores the server's trailer metadata into *md when the call ends.
func Trailer(md *metadata.MD) CallOption {
	return callOptionFunc(func(ci *callInfo) { ci.trailer = md })
}
