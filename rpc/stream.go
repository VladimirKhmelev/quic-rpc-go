package rpc

import (
	"context"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
)

// ClientStream is the client side of a streaming call. SendMsg and RecvMsg
// may run in two different goroutines, but SendMsg must not be called
// concurrently with SendMsg, nor RecvMsg with RecvMsg.
type ClientStream interface {
	// Header waits for the server's header metadata. It returns nil, nil if
	// the call ended without header metadata; RecvMsg then reports the status.
	Header() (metadata.MD, error)
	// Trailer returns the server's trailer metadata. It is valid only after
	// RecvMsg returned an error, including io.EOF.
	Trailer() metadata.MD
	// CloseSend closes the sending direction; the server's next receive
	// returns io.EOF.
	CloseSend() error
	// Context returns the context of the call.
	Context() context.Context
	// SendMsg sends a message. Once the call is over it returns io.EOF and
	// RecvMsg reports the status.
	SendMsg(m any) error
	// RecvMsg receives a message into m. It returns io.EOF when the call
	// ended with OK and a status error otherwise. Once the call's context is
	// canceled it returns Canceled or DeadlineExceeded, even if a message
	// had already arrived.
	RecvMsg(m any) error
}

// ServerStream is the server side of a streaming call.
type ServerStream interface {
	// SetHeader adds header metadata, sent with the first message or with
	// SendHeader. It fails once the header was sent.
	SetHeader(md metadata.MD) error
	// SendHeader sends the header metadata, adding md to it.
	SendHeader(md metadata.MD) error
	// SetTrailer adds trailer metadata, sent when the handler returns.
	SetTrailer(md metadata.MD)
	// Context returns the context of the call: it carries the deadline and
	// the incoming metadata and is canceled when the call ends.
	Context() context.Context
	// SendMsg sends a message.
	SendMsg(m any) error
	// RecvMsg receives a message into m; io.EOF means the client closed its
	// sending direction.
	RecvMsg(m any) error
}

// The typed stream interfaces below are those of grpc-go, one pair per kind
// of streaming call; generated code uses them.

// ServerStreamingClient is the typed client side of a server-streaming
// call: the request went out when the call started.
type ServerStreamingClient[Res any] interface {
	Recv() (*Res, error)
	ClientStream
}

// ServerStreamingServer is the typed server side of a server-streaming
// call: the handler got the request as an argument.
type ServerStreamingServer[Res any] interface {
	Send(*Res) error
	ServerStream
}

// ClientStreamingClient is the typed client side of a client-streaming
// call.
type ClientStreamingClient[Req any, Res any] interface {
	Send(*Req) error
	// CloseAndRecv closes the sending direction and receives the response.
	CloseAndRecv() (*Res, error)
	ClientStream
}

// ClientStreamingServer is the typed server side of a client-streaming
// call.
type ClientStreamingServer[Req any, Res any] interface {
	Recv() (*Req, error)
	// SendAndClose sends the response; the call ends when the handler
	// returns.
	SendAndClose(*Res) error
	ServerStream
}

// BidiStreamingClient is the typed client side of a bidirectional call.
type BidiStreamingClient[Req any, Res any] interface {
	Send(*Req) error
	Recv() (*Res, error)
	ClientStream
}

// BidiStreamingServer is the typed server side of a bidirectional call.
type BidiStreamingServer[Req any, Res any] interface {
	Recv() (*Req, error)
	Send(*Res) error
	ServerStream
}

// GenericClientStream implements the typed client stream interfaces on top
// of a ClientStream. Generated code uses it.
type GenericClientStream[Req any, Res any] struct {
	ClientStream
}

var (
	_ ServerStreamingClient[int]      = (*GenericClientStream[int, int])(nil)
	_ ClientStreamingClient[int, int] = (*GenericClientStream[int, int])(nil)
	_ BidiStreamingClient[int, int]   = (*GenericClientStream[int, int])(nil)
	_ ServerStreamingServer[int]      = (*GenericServerStream[int, int])(nil)
	_ ClientStreamingServer[int, int] = (*GenericServerStream[int, int])(nil)
	_ BidiStreamingServer[int, int]   = (*GenericServerStream[int, int])(nil)
)

// Send sends a request message.
func (x *GenericClientStream[Req, Res]) Send(m *Req) error {
	return x.SendMsg(m)
}

// Recv receives a response message.
func (x *GenericClientStream[Req, Res]) Recv() (*Res, error) {
	m := new(Res)
	if err := x.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// CloseAndRecv closes the sending direction and receives the only response
// of a client-streaming call.
func (x *GenericClientStream[Req, Res]) CloseAndRecv() (*Res, error) {
	if err := x.CloseSend(); err != nil {
		return nil, err
	}
	m := new(Res)
	if err := x.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// GenericServerStream implements the typed server stream interfaces on top
// of a ServerStream. Generated code uses it.
type GenericServerStream[Req any, Res any] struct {
	ServerStream
}

// Recv receives a request message.
func (x *GenericServerStream[Req, Res]) Recv() (*Req, error) {
	m := new(Req)
	if err := x.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}

// Send sends a response message.
func (x *GenericServerStream[Req, Res]) Send(m *Res) error {
	return x.SendMsg(m)
}

// SendAndClose sends the only response of a client-streaming call; the call
// ends when the handler returns.
func (x *GenericServerStream[Req, Res]) SendAndClose(m *Res) error {
	return x.SendMsg(m)
}
