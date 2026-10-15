package rpcquic

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/quic-go/quic-go"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// sameError reports whether got matches want: transport errors field by
// field, anything else must be returned unchanged.
func sameError(got, want error) bool {
	if ws, ok := errors.AsType[*transport.StreamError](want); ok {
		g, ok := errors.AsType[*transport.StreamError](got)
		return ok && *g == *ws
	}
	if wc, ok := errors.AsType[*transport.ConnError](want); ok {
		g, ok := errors.AsType[*transport.ConnError](got)
		return ok && g.Code == wc.Code && g.Remote == wc.Remote && g.Reason == wc.Reason && errors.Is(g.Err, wc.Err)
	}
	// Identity, not errors.Is: callers such as io.ReadAll compare with ==.
	return got == want //nolint:errorlint // pass-through must return the very same value
}

func TestConvertError(t *testing.T) {
	var (
		other     = errors.New("other")
		idle      = &quic.IdleTimeoutError{}
		handshake = &quic.HandshakeTimeoutError{}
		reset     = &quic.StatelessResetError{}
		transErr  = &quic.TransportError{ErrorCode: quic.InternalError}
	)
	tests := []struct {
		name string
		in   error
		want error
	}{
		{"nil", nil, nil},
		{"EOF", io.EOF, io.EOF},
		{"context", context.DeadlineExceeded, context.DeadlineExceeded},
		{"other", other, other},
		{"remote reset", &quic.StreamError{ErrorCode: 1, Remote: true},
			&transport.StreamError{Code: transport.Cancelled, Remote: true}},
		{"local reset", &quic.StreamError{ErrorCode: 2},
			&transport.StreamError{Code: transport.ProtocolError}},
		{"application close", &quic.ApplicationError{ErrorCode: 3, Remote: true, ErrorMessage: "bye"},
			&transport.ConnError{Code: transport.InternalError, Remote: true, Reason: "bye"}},
		{"idle timeout", idle, &transport.ConnError{Err: idle}},
		{"handshake timeout", handshake, &transport.ConnError{Err: handshake}},
		{"stateless reset", reset, &transport.ConnError{Err: reset}},
		{"transport error", transErr, &transport.ConnError{Err: transErr}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := convertError(tt.in); !sameError(got, tt.want) {
				t.Fatalf("convertError(%v) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}

func TestStreamCause(t *testing.T) {
	tests := []struct {
		name string
		in   error
		want error
	}{
		{"closed", context.Canceled, transport.ErrStreamClosed},
		{"reset", &quic.StreamError{ErrorCode: 1, Remote: true}, &transport.StreamError{Code: transport.Cancelled, Remote: true}},
		{"connection closed", &quic.ApplicationError{ErrorCode: 0}, &transport.ConnError{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := streamCause(tt.in); !sameError(got, tt.want) {
				t.Fatalf("streamCause(%v) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}
