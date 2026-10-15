package transport_test

import (
	"errors"
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

func TestErrorCodeString(t *testing.T) {
	tests := map[transport.ErrorCode]string{
		transport.NoError:       "NO_ERROR",
		transport.Cancelled:     "CANCELLED",
		transport.ProtocolError: "PROTOCOL_ERROR",
		transport.InternalError: "INTERNAL_ERROR",
		0x2a:                    "ErrorCode(0x2a)",
	}
	for code, want := range tests {
		if got := code.String(); got != want {
			t.Errorf("ErrorCode(%d).String() = %q, want %q", uint64(code), got, want)
		}
	}
}

func TestErrorMessages(t *testing.T) {
	cause := errors.New("timeout: no recent network activity")
	tests := []struct {
		err  error
		want string
	}{
		{&transport.StreamError{Code: transport.Cancelled, Remote: true},
			"transport: stream reset by remote side: CANCELLED"},
		{&transport.StreamError{Code: transport.ProtocolError},
			"transport: stream reset by local side: PROTOCOL_ERROR"},
		{&transport.ConnError{Code: transport.InternalError, Remote: true, Reason: "bye"},
			`transport: connection closed by remote side: INTERNAL_ERROR "bye"`},
		{&transport.ConnError{Err: cause},
			"transport: connection closed: timeout: no recent network activity"},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("Error() = %q, want %q", got, tt.want)
		}
	}
	if err := error(&transport.ConnError{Err: cause}); !errors.Is(err, cause) {
		t.Error("ConnError does not unwrap to its cause")
	}
}
