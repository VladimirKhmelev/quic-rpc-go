package status_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	istatus "github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/status"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

func TestNewAndErr(t *testing.T) {
	s := status.Newf(codes.NotFound, "chat %d not found", 7)
	if s.Code() != codes.NotFound || s.Message() != "chat 7 not found" {
		t.Fatalf("got %v %q", s.Code(), s.Message())
	}
	err := s.Err()
	if err == nil {
		t.Fatal("Err() of a NotFound status is nil")
	}
	if want := "rpc error: code = NotFound desc = chat 7 not found"; err.Error() != want {
		t.Fatalf("Error() = %q, want %q", err.Error(), want)
	}
	if status.Error(codes.OK, "fine") != nil || status.Errorf(codes.OK, "%s", "fine") != nil {
		t.Fatal("an OK status produced a non-nil error")
	}
	if err := status.Errorf(codes.Internal, "bad %s", "state"); status.Code(err) != codes.Internal {
		t.Fatalf("Errorf code = %v", status.Code(err))
	}
}

func TestNilStatusIsOK(t *testing.T) {
	var s *status.Status
	if s.Code() != codes.OK || s.Message() != "" || s.Err() != nil || s.Details() != nil {
		t.Fatalf("nil status: code %v, message %q, err %v, details %v", s.Code(), s.Message(), s.Err(), s.Details())
	}
}

type nilStatusError struct{}

func (nilStatusError) Error() string              { return "carries no status" }
func (nilStatusError) RPCStatus() *istatus.Status { return nil }

func TestFromError(t *testing.T) {
	detailed, err := status.New(codes.Aborted, "conflict").WithDetails(wrapperspb.String("retry"))
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	statusErr := detailed.Err()

	t.Run("nil", func(t *testing.T) {
		if s, ok := status.FromError(nil); s != nil || !ok {
			t.Fatalf("FromError(nil) = %v, %v; want nil, true", s, ok)
		}
	})
	t.Run("status error", func(t *testing.T) {
		if s, ok := status.FromError(statusErr); s != detailed || !ok {
			t.Fatalf("FromError = %v, %v; want the original status", s, ok)
		}
	})
	t.Run("wrapped status error", func(t *testing.T) {
		wrapped := fmt.Errorf("load chat: %w", statusErr)
		s, ok := status.FromError(wrapped)
		if !ok || s.Code() != codes.Aborted || s.Message() != wrapped.Error() || len(s.Details()) != 1 {
			t.Fatalf("FromError = %v (%d details), %v; want Aborted with the wrapped message and the details", s, len(s.Details()), ok)
		}
	})
	t.Run("plain error", func(t *testing.T) {
		s, ok := status.FromError(errors.New("boom"))
		if ok || s.Code() != codes.Unknown || s.Message() != "boom" {
			t.Fatalf("FromError = %v, %v; want Unknown \"boom\", false", s, ok)
		}
	})
	t.Run("error with nil status", func(t *testing.T) {
		s, ok := status.FromError(nilStatusError{})
		if ok || s.Code() != codes.Unknown {
			t.Fatalf("FromError = %v, %v; want Unknown, false", s, ok)
		}
	})
}

func TestCodeAndConvert(t *testing.T) {
	tests := []struct {
		err  error
		want codes.Code
	}{
		{nil, codes.OK},
		{status.Error(codes.Unavailable, "down"), codes.Unavailable},
		{fmt.Errorf("wrapped: %w", status.Error(codes.PermissionDenied, "no")), codes.PermissionDenied},
		{errors.New("plain"), codes.Unknown},
	}
	for _, tt := range tests {
		if got := status.Code(tt.err); got != tt.want {
			t.Errorf("Code(%v) = %v, want %v", tt.err, got, tt.want)
		}
		if got := status.Convert(tt.err).Code(); got != tt.want {
			t.Errorf("Convert(%v).Code() = %v, want %v", tt.err, got, tt.want)
		}
	}
}

func TestFromContextError(t *testing.T) {
	tests := []struct {
		err  error
		want codes.Code
	}{
		{context.DeadlineExceeded, codes.DeadlineExceeded},
		{context.Canceled, codes.Canceled},
		{fmt.Errorf("query: %w", context.Canceled), codes.Canceled},
		{errors.New("other"), codes.Unknown},
	}
	for _, tt := range tests {
		s := status.FromContextError(tt.err)
		if s.Code() != tt.want || s.Message() != tt.err.Error() {
			t.Errorf("FromContextError(%v) = %v, want code %v with the error's message", tt.err, s, tt.want)
		}
	}
	if s := status.FromContextError(nil); s != nil {
		t.Errorf("FromContextError(nil) = %v, want nil", s)
	}
}

func TestWithDetails(t *testing.T) {
	if _, err := status.New(codes.OK, "").WithDetails(wrapperspb.String("x")); err == nil {
		t.Fatal("WithDetails on an OK status succeeded")
	}

	base, err := status.New(codes.FailedPrecondition, "locked").WithDetails(wrapperspb.String("first"))
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	a, err := base.WithDetails(wrapperspb.String("a"))
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}
	b, err := base.WithDetails(wrapperspb.Int64(2))
	if err != nil {
		t.Fatalf("WithDetails: %v", err)
	}

	if got := len(base.Details()); got != 1 {
		t.Fatalf("base has %d details after deriving from it, want 1", got)
	}
	wantA := []proto.Message{wrapperspb.String("first"), wrapperspb.String("a")}
	wantB := []proto.Message{wrapperspb.String("first"), wrapperspb.Int64(2)}
	for name, tc := range map[string]struct {
		s    *status.Status
		want []proto.Message
	}{"a": {a, wantA}, "b": {b, wantB}} {
		got := tc.s.Details()
		if len(got) != len(tc.want) {
			t.Fatalf("%s: %d details, want %d", name, len(got), len(tc.want))
		}
		for i := range got {
			m, ok := got[i].(proto.Message)
			if !ok || !proto.Equal(m, tc.want[i]) {
				t.Errorf("%s: detail %d = %v, want %v", name, i, got[i], tc.want[i])
			}
		}
	}

	// proto3 strings must be valid UTF-8, so this detail cannot be packed.
	if _, err := base.WithDetails(wrapperspb.String("\xff")); err == nil {
		t.Error("WithDetails packed a detail that cannot be marshaled")
	}
}

func TestErrorIs(t *testing.T) {
	withDetail := func(d string) error {
		s, err := status.New(codes.NotFound, "missing").WithDetails(wrapperspb.String(d))
		if err != nil {
			t.Fatalf("WithDetails: %v", err)
		}
		return s.Err()
	}
	tests := []struct {
		name   string
		err    error
		target error
		want   bool
	}{
		{"equal statuses", status.Error(codes.NotFound, "missing"), status.Error(codes.NotFound, "missing"), true},
		{"different code", status.Error(codes.NotFound, "missing"), status.Error(codes.Internal, "missing"), false},
		{"different message", status.Error(codes.NotFound, "missing"), status.Error(codes.NotFound, "gone"), false},
		{"equal details", withDetail("x"), withDetail("x"), true},
		{"different details", withDetail("x"), withDetail("y"), false},
		{"missing details", withDetail("x"), status.Error(codes.NotFound, "missing"), false},
		{"wrapped", fmt.Errorf("w: %w", status.Error(codes.NotFound, "missing")), status.Error(codes.NotFound, "missing"), true},
		{"not a status", status.Error(codes.NotFound, "missing"), errors.New("missing"), false},
	}
	for _, tt := range tests {
		if got := errors.Is(tt.err, tt.target); got != tt.want {
			t.Errorf("%s: errors.Is = %v, want %v", tt.name, got, tt.want)
		}
	}
}
