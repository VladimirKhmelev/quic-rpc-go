// Package status creates and inspects call statuses. It mirrors the API of
// grpc-go's status package: a handler returns status.Error(codes.NotFound,
// "...") and the client gets the same code and message back.
package status

import (
	"context"
	"errors"
	"fmt"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	istatus "github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/status"
)

// Status is the outcome of a call: a code, a message and optional details.
// A Status is immutable; a nil *Status means OK.
type Status = istatus.Status

// New returns a status with the given code and message.
func New(c codes.Code, msg string) *Status {
	return istatus.New(c, msg)
}

// Newf returns a status with the given code and a formatted message.
func Newf(c codes.Code, format string, a ...any) *Status {
	return New(c, fmt.Sprintf(format, a...))
}

// Error returns an error carrying the code and message, or nil if c is OK.
func Error(c codes.Code, msg string) error {
	return New(c, msg).Err()
}

// Errorf returns an error carrying the code and a formatted message, or nil
// if c is OK.
func Errorf(c codes.Code, format string, a ...any) error {
	return Error(c, fmt.Sprintf(format, a...))
}

// rpcStatusError is an error that carries a status, such as the errors
// returned by Error and Status.Err.
type rpcStatusError interface {
	error
	RPCStatus() *Status
}

// FromError returns the status carried by err and true. If err wraps such an
// error, the status keeps its code and details but takes err's full message.
// For nil it returns nil (OK) and true; for any other error, a status with
// code Unknown and err's message, and false.
func FromError(err error) (s *Status, ok bool) {
	if err == nil {
		return nil, true
	}
	se, ok := errors.AsType[rpcStatusError](err)
	if !ok || se.RPCStatus() == nil {
		return New(codes.Unknown, err.Error()), false
	}
	st := se.RPCStatus()
	if se == err { //nolint:errorlint // telling a status error from an error that wraps it
		return st, true
	}
	c, _, details := istatus.Wire(st)
	return istatus.FromWire(c, err.Error(), details), true
}

// Convert is like FromError but discards the boolean.
func Convert(err error) *Status {
	s, _ := FromError(err)
	return s
}

// Code returns the code of the status carried by err: OK for nil and
// Unknown for errors that carry no status.
func Code(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	return Convert(err).Code()
}

// FromContextError converts a context error to a status: DeadlineExceeded
// and Canceled for the context errors (also when wrapped), Unknown for
// anything else, nil for nil.
func FromContextError(err error) *Status {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.DeadlineExceeded):
		return New(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled):
		return New(codes.Canceled, err.Error())
	default:
		return New(codes.Unknown, err.Error())
	}
}
