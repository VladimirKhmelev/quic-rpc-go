package call

import (
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	istatus "github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/status"
)

// ErrProtocol reports that the peer broke the frame grammar. The stream must
// be reset with transport.ProtocolError.
var ErrProtocol = errors.New("protocol violation")

// errNoTrailers reports that the server finished its side without TRAILERS.
var errNoTrailers = errors.New("server closed the stream without sending trailers")

func protocolError(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrProtocol, fmt.Sprintf(format, a...))
}

// RequestHeaders is the HEADERS frame that opened a call.
type RequestHeaders struct {
	Method string
	// Timeout is 0 if the client set no deadline.
	Timeout time.Duration
	MD      map[string][]string
}

// ReadRequestHeaders reads the first frame of a call on the server side.
func ReadRequestHeaders(fr *frame.Reader) (RequestHeaders, error) {
	f, err := fr.ReadFrame()
	if err != nil {
		return RequestHeaders{}, readError(err)
	}
	if f.Type != frame.TypeHeaders {
		return RequestHeaders{}, protocolError("call starts with %v, want HEADERS", f.Type)
	}
	h, err := frame.DecodeHeaders(f.Payload)
	if err != nil {
		return RequestHeaders{}, fmt.Errorf("%w: %w", ErrProtocol, err)
	}
	if h.GetMethod() == "" {
		return RequestHeaders{}, protocolError("HEADERS that open a call have no method")
	}
	return RequestHeaders{
		Method:  h.GetMethod(),
		Timeout: time.Duration(h.GetTimeoutUs()) * time.Microsecond, //nolint:gosec // DecodeHeaders caps it at frame.MaxTimeoutMicros
		MD:      frame.DecodeMetadata(h.GetMetadata()),
	}, nil
}

// readError turns framing errors into protocol errors. An oversized MESSAGE
// stays a *frame.TooLargeError: it ends the call with ResourceExhausted, as
// in gRPC. Transport errors and io.EOF pass through.
func readError(err error) error {
	if tl, ok := errors.AsType[*frame.TooLargeError](err); ok && tl.Type == frame.TypeMessage {
		return err
	}
	for _, framing := range []error{io.ErrUnexpectedEOF, frame.ErrUnknownType, frame.ErrNonMinimal, frame.ErrMalformed, frame.ErrTooLarge} {
		if errors.Is(err, framing) {
			return fmt.Errorf("%w: %w", ErrProtocol, err)
		}
	}
	return err
}

// ServerRecv reads what a client sends after HEADERS: MESSAGE frames, then FIN.
type ServerRecv struct {
	fr  *frame.Reader
	eof bool
}

// NewServerRecv returns a ServerRecv reading from fr.
func NewServerRecv(fr *frame.Reader) *ServerRecv { return &ServerRecv{fr: fr} }

// Next returns the next message payload, or io.EOF after the client's FIN.
func (r *ServerRecv) Next() ([]byte, error) {
	if r.eof {
		return nil, io.EOF
	}
	f, err := r.fr.ReadFrame()
	if errors.Is(err, io.EOF) {
		r.eof = true
		return nil, io.EOF
	}
	if err != nil {
		return nil, readError(err)
	}
	if f.Type != frame.TypeMessage {
		return nil, protocolError("client sent %v after HEADERS", f.Type)
	}
	return f.Payload, nil
}

// Done reports whether the client's FIN was read.
func (r *ServerRecv) Done() bool { return r.eof }

// ClientRecv reads what a server sends: [HEADERS] MESSAGE* TRAILERS FIN.
type ClientRecv struct {
	fr       *frame.Reader
	onHeader func()

	headerKnown bool
	header      map[string][]string
	pending     []byte
	hasPending  bool

	ended   bool
	status  *istatus.Status
	trailer map[string][]string
}

// NewClientRecv returns a ClientRecv reading from fr. onHeader runs once,
// inside the reading call, as soon as HeaderKnown becomes true.
func NewClientRecv(fr *frame.Reader, onHeader func()) *ClientRecv {
	return &ClientRecv{fr: fr, onHeader: onHeader}
}

func (r *ClientRecv) markHeaderKnown() {
	if !r.headerKnown {
		r.headerKnown = true
		r.onHeader()
	}
}

// HeaderKnown reports whether it is known what header metadata the server
// sent: its HEADERS frame, or another frame that proves there is none.
func (r *ClientRecv) HeaderKnown() bool { return r.headerKnown }

// Header returns the server's header metadata; nil if it sent none.
func (r *ClientRecv) Header() map[string][]string { return r.header }

// Status returns the status from TRAILERS once Next returned io.EOF.
func (r *ClientRecv) Status() *istatus.Status { return r.status }

// Trailer returns the trailer metadata once Next returned io.EOF.
func (r *ClientRecv) Trailer() map[string][]string { return r.trailer }

// ReadHeader reads until HeaderKnown is true. A message read on the way is
// kept for the next call to Next.
func (r *ClientRecv) ReadHeader() error {
	for !r.headerKnown {
		payload, err := r.next()
		if err != nil {
			return err
		}
		if payload != nil {
			r.pending, r.hasPending = payload, true
		}
	}
	return nil
}

// Next returns the next message payload. It returns io.EOF once TRAILERS
// and FIN were read; Status and Trailer then hold the outcome.
func (r *ClientRecv) Next() ([]byte, error) {
	if r.hasPending {
		p := r.pending
		r.pending, r.hasPending = nil, false
		return p, nil
	}
	for {
		payload, err := r.next()
		if err != nil || payload != nil {
			return payload, err
		}
	}
}

// next reads one frame. It returns a payload for MESSAGE, (nil, nil) after
// HEADERS and io.EOF at the end of the call.
func (r *ClientRecv) next() ([]byte, error) {
	if r.ended {
		return nil, io.EOF
	}
	f, err := r.fr.ReadFrame()
	if errors.Is(err, io.EOF) {
		return nil, errNoTrailers
	}
	if err != nil {
		return nil, readError(err)
	}
	switch f.Type {
	case frame.TypeHeaders:
		if r.headerKnown {
			return nil, protocolError("HEADERS after the start of the response")
		}
		h, err := frame.DecodeHeaders(f.Payload)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrProtocol, err)
		}
		if h.GetMethod() != "" || h.GetTimeoutUs() != 0 {
			return nil, protocolError("server HEADERS carry a method or a timeout")
		}
		r.header = frame.DecodeMetadata(h.GetMetadata())
		r.markHeaderKnown()
		return nil, nil
	case frame.TypeMessage:
		r.markHeaderKnown()
		return f.Payload, nil
	case frame.TypeTrailers:
		r.markHeaderKnown()
		t, err := frame.DecodeTrailers(f.Payload)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrProtocol, err)
		}
		if _, err := r.fr.ReadFrame(); !errors.Is(err, io.EOF) {
			if err == nil {
				return nil, protocolError("frames after TRAILERS")
			}
			return nil, readError(err)
		}
		r.ended = true
		r.status = istatus.FromWire(codes.Code(t.GetCode()), t.GetMessage(), t.GetDetails())
		r.trailer = frame.DecodeMetadata(t.GetMetadata())
		return nil, io.EOF
	default:
		return nil, protocolError("server sent %v on a call", f.Type)
	}
}
