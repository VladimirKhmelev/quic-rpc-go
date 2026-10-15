// Package status implements the Status type. Package rpc/status exposes it
// to users; the call layer uses FromWire and Wire to move statuses over the
// network with their details exactly as received, including detail types
// this process does not know.
package status

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
)

// Status is the outcome of a call: a code, a message and optional details.
// A Status is immutable. A nil *Status means OK.
type Status struct {
	code    codes.Code
	message string
	details []*anypb.Any
}

// New returns a status with the given code and message.
func New(c codes.Code, msg string) *Status {
	return &Status{code: c, message: msg}
}

// FromWire returns a status built from the fields of a TRAILERS frame. It
// takes ownership of details.
func FromWire(c codes.Code, msg string, details []*anypb.Any) *Status {
	return &Status{code: c, message: msg, details: details}
}

// Wire returns the fields to put into a TRAILERS frame. The details must
// not be modified.
func Wire(s *Status) (codes.Code, string, []*anypb.Any) {
	if s == nil {
		return codes.OK, "", nil
	}
	return s.code, s.message, s.details
}

// Code returns the status code.
func (s *Status) Code() codes.Code {
	if s == nil {
		return codes.OK
	}
	return s.code
}

// Message returns the status message.
func (s *Status) Message() string {
	if s == nil {
		return ""
	}
	return s.message
}

// Err returns an error carrying the status, or nil if the code is OK.
func (s *Status) Err() error {
	if s.Code() == codes.OK {
		return nil
	}
	return &Error{s: s}
}

// WithDetails returns a copy of s with details appended. It fails for an
// OK status, which cannot carry details.
func (s *Status) WithDetails(details ...proto.Message) (*Status, error) {
	if s.Code() == codes.OK {
		return nil, errors.New("status: no error details for status with code OK")
	}
	out := &Status{code: s.code, message: s.message, details: make([]*anypb.Any, len(s.details), len(s.details)+len(details))}
	copy(out.details, s.details)
	for _, d := range details {
		a, err := anypb.New(d)
		if err != nil {
			return nil, fmt.Errorf("status: pack detail: %w", err)
		}
		out.details = append(out.details, a)
	}
	return out, nil
}

// Details returns the unpacked details. A detail whose type is not linked
// into the program is returned as the error that prevented unpacking it.
func (s *Status) Details() []any {
	if s == nil {
		return nil
	}
	out := make([]any, 0, len(s.details))
	for _, a := range s.details {
		m, err := a.UnmarshalNew()
		if err != nil {
			out = append(out, err)
			continue
		}
		out = append(out, m)
	}
	return out
}

func (s *Status) String() string {
	return fmt.Sprintf("rpc error: code = %s desc = %s", s.Code(), s.Message())
}

// Error is the error form of a non-OK Status.
type Error struct {
	s *Status
}

func (e *Error) Error() string { return e.s.String() }

// RPCStatus returns the status carried by the error.
func (e *Error) RPCStatus() *Status { return e.s }

// Is reports whether target is an *Error with an equal status: same code,
// message and details.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error) //nolint:errorlint // Is compares this exact error, it must not unwrap target
	if !ok {
		return false
	}
	a, b := e.s, t.s
	if a.Code() != b.Code() || a.Message() != b.Message() || len(a.details) != len(b.details) {
		return false
	}
	for i := range a.details {
		if !proto.Equal(a.details[i], b.details[i]) {
			return false
		}
	}
	return true
}
