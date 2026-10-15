package codes_test

import (
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
)

// TestNumbering pins every code to its gRPC number and name.
func TestNumbering(t *testing.T) {
	tests := []struct {
		code codes.Code
		num  uint32
		name string
	}{
		{codes.OK, 0, "OK"},
		{codes.Canceled, 1, "Canceled"},
		{codes.Unknown, 2, "Unknown"},
		{codes.InvalidArgument, 3, "InvalidArgument"},
		{codes.DeadlineExceeded, 4, "DeadlineExceeded"},
		{codes.NotFound, 5, "NotFound"},
		{codes.AlreadyExists, 6, "AlreadyExists"},
		{codes.PermissionDenied, 7, "PermissionDenied"},
		{codes.ResourceExhausted, 8, "ResourceExhausted"},
		{codes.FailedPrecondition, 9, "FailedPrecondition"},
		{codes.Aborted, 10, "Aborted"},
		{codes.OutOfRange, 11, "OutOfRange"},
		{codes.Unimplemented, 12, "Unimplemented"},
		{codes.Internal, 13, "Internal"},
		{codes.Unavailable, 14, "Unavailable"},
		{codes.DataLoss, 15, "DataLoss"},
		{codes.Unauthenticated, 16, "Unauthenticated"},
	}
	for _, tt := range tests {
		if uint32(tt.code) != tt.num {
			t.Errorf("%s = %d, want %d", tt.name, uint32(tt.code), tt.num)
		}
		if got := tt.code.String(); got != tt.name {
			t.Errorf("Code(%d).String() = %q, want %q", tt.num, got, tt.name)
		}
	}
	if got := codes.Code(17).String(); got != "Code(17)" {
		t.Errorf("Code(17).String() = %q, want %q", got, "Code(17)")
	}
}
