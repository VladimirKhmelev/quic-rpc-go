package rpc

import "github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"

// CallOption configures a single call.
type CallOption interface {
	apply(*callInfo)
}

type callInfo struct {
	header     *metadata.MD
	trailer    *metadata.MD
	idempotent bool
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

// Idempotent permits early transmission of this call when the transport has
// 0-RTT enabled. The entire call, including metadata and interceptor effects,
// must tolerate replay. For streams this applies to every message. The server
// must also mark the method idempotent to execute it before its handshake ends.
// Passing false overrides a generated default; the last option wins.
func Idempotent(enabled bool) CallOption {
	return callOptionFunc(func(ci *callInfo) { ci.idempotent = enabled })
}
