package call

import (
	"errors"
	"fmt"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	istatus "github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/status"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// ClientStatus maps an error that ended a call on the client side to the
// status the caller sees. The caller handles its own context's errors first.
func ClientStatus(err error) *istatus.Status {
	if tl, ok := errors.AsType[*frame.TooLargeError](err); ok {
		return istatus.New(codes.ResourceExhausted,
			fmt.Sprintf("received message larger than max (%d vs. %d)", tl.Size, tl.Limit))
	}
	if ce, ok := errors.AsType[*transport.ConnError](err); ok {
		return istatus.New(codes.Unavailable, ce.Error())
	}
	if se, ok := errors.AsType[*transport.StreamError](err); ok {
		if se.Code == transport.Cancelled {
			return istatus.New(codes.Canceled, se.Error())
		}
		return istatus.New(codes.Internal, se.Error())
	}
	// Protocol violations, a missing TRAILERS frame and anything unexpected.
	return istatus.New(codes.Internal, err.Error())
}

// ResetCode returns the code to reset a stream with after err.
func ResetCode(err error) transport.ErrorCode {
	if errors.Is(err, ErrProtocol) {
		return transport.ProtocolError
	}
	return transport.Cancelled
}
