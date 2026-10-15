// Package codes defines the status codes of quic-rpc-go. They have the same
// names, numbers and meaning as gRPC status codes.
package codes

import "strconv"

// Code is a status code of a call.
type Code uint32

// Status codes, numbered as in gRPC.
const (
	// OK means the call succeeded.
	OK Code = 0
	// Canceled means the call was canceled, typically by the caller.
	Canceled Code = 1
	// Unknown means an error without a more specific code, such as a plain
	// error returned by a handler.
	Unknown Code = 2
	// InvalidArgument means the client passed an invalid argument.
	InvalidArgument Code = 3
	// DeadlineExceeded means the deadline expired before the call completed.
	DeadlineExceeded Code = 4
	// NotFound means a requested entity was not found.
	NotFound Code = 5
	// AlreadyExists means an entity the client tried to create already exists.
	AlreadyExists Code = 6
	// PermissionDenied means the caller may not perform the operation.
	PermissionDenied Code = 7
	// ResourceExhausted means a resource or quota ran out, for example a
	// message exceeded the size limit.
	ResourceExhausted Code = 8
	// FailedPrecondition means the system is not in a state required for
	// the operation.
	FailedPrecondition Code = 9
	// Aborted means the operation was aborted, typically by a concurrency
	// conflict.
	Aborted Code = 10
	// OutOfRange means the operation went past the valid range.
	OutOfRange Code = 11
	// Unimplemented means the method is not implemented or not supported.
	Unimplemented Code = 12
	// Internal means an invariant expected by the system was broken.
	Internal Code = 13
	// Unavailable means the service is currently unavailable, for example
	// because the connection was lost; retrying may help.
	Unavailable Code = 14
	// DataLoss means unrecoverable data loss or corruption.
	DataLoss Code = 15
	// Unauthenticated means the caller has no valid credentials.
	Unauthenticated Code = 16
)

var names = [...]string{
	OK:                 "OK",
	Canceled:           "Canceled",
	Unknown:            "Unknown",
	InvalidArgument:    "InvalidArgument",
	DeadlineExceeded:   "DeadlineExceeded",
	NotFound:           "NotFound",
	AlreadyExists:      "AlreadyExists",
	PermissionDenied:   "PermissionDenied",
	ResourceExhausted:  "ResourceExhausted",
	FailedPrecondition: "FailedPrecondition",
	Aborted:            "Aborted",
	OutOfRange:         "OutOfRange",
	Unimplemented:      "Unimplemented",
	Internal:           "Internal",
	Unavailable:        "Unavailable",
	DataLoss:           "DataLoss",
	Unauthenticated:    "Unauthenticated",
}

func (c Code) String() string {
	if int(c) < len(names) {
		return names[c]
	}
	return "Code(" + strconv.FormatUint(uint64(c), 10) + ")"
}
