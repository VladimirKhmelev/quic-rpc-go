// Package frame implements the wire framing of the RPC protocol.
//
// A frame is a QUIC variable-length integer type, a QUIC variable-length
// integer payload length and the payload itself (RFC 9000, section 16).
// Both integers must use the shortest encoding. The layout and the order of
// frames on a stream are described in docs/protocol.md.
package frame

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/quic-go/quic-go/quicvarint"
	"google.golang.org/protobuf/proto"
)

// Type identifies the kind of a frame.
type Type uint64

// Frame types. Any other type is a protocol error.
const (
	TypeMessage  Type = 0x00
	TypeHeaders  Type = 0x01
	TypeTrailers Type = 0x02
)

func (t Type) String() string {
	switch t {
	case TypeMessage:
		return "MESSAGE"
	case TypeHeaders:
		return "HEADERS"
	case TypeTrailers:
		return "TRAILERS"
	default:
		return fmt.Sprintf("Type(%#x)", uint64(t))
	}
}

// Default payload limits. The message limit matches grpc-go's default
// maximum receive message size.
const (
	DefaultMaxMessageSize = 4 << 20
	DefaultMaxHeadersSize = 64 << 10
)

// Limits bounds the payload size a Reader accepts.
type Limits struct {
	// MaxMessageSize bounds MESSAGE payloads.
	MaxMessageSize uint64
	// MaxHeadersSize bounds HEADERS and TRAILERS payloads.
	MaxHeadersSize uint64
}

// DefaultLimits returns the default payload limits.
func DefaultLimits() Limits {
	return Limits{MaxMessageSize: DefaultMaxMessageSize, MaxHeadersSize: DefaultMaxHeadersSize}
}

// Errors returned by Reader and the payload decoders. Truncated input is
// reported as io.ErrUnexpectedEOF.
var (
	ErrUnknownType = errors.New("frame: unknown frame type")
	ErrTooLarge    = errors.New("frame: payload exceeds limit")
	ErrNonMinimal  = errors.New("frame: varint is not minimally encoded")
	ErrMalformed   = errors.New("frame: malformed payload")
)

// Frame is a decoded frame.
type Frame struct {
	Type    Type
	Payload []byte
}

// Reader reads frames from a byte stream.
type Reader struct {
	br     *bufio.Reader
	limits Limits
}

// NewReader returns a Reader that reads frames from r and rejects payloads
// larger than limits allow.
func NewReader(r io.Reader, limits Limits) *Reader {
	return &Reader{br: bufio.NewReader(r), limits: limits}
}

// ReadFrame reads the next frame. It returns io.EOF if the stream ends
// cleanly between frames and io.ErrUnexpectedEOF if it ends inside one.
// Errors of the underlying reader are returned unchanged. The payload size
// is checked against the limits before any memory is allocated for it, and
// the returned payload is owned by the caller.
func (r *Reader) ReadFrame() (Frame, error) {
	rawType, err := r.readVarint()
	if err != nil {
		return Frame{}, err
	}
	t := Type(rawType)
	limit, ok := r.limit(t)
	if !ok {
		return Frame{}, fmt.Errorf("%w: %v", ErrUnknownType, t)
	}
	n, err := r.readVarint()
	if err != nil {
		return Frame{}, unexpectedEOF(err)
	}
	if n > limit {
		return Frame{}, fmt.Errorf("%w: %v payload of %d bytes, limit %d", ErrTooLarge, t, n, limit)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r.br, payload); err != nil {
		return Frame{}, unexpectedEOF(err)
	}
	return Frame{Type: t, Payload: payload}, nil
}

func (r *Reader) limit(t Type) (uint64, bool) {
	switch t {
	case TypeMessage:
		return r.limits.MaxMessageSize, true
	case TypeHeaders, TypeTrailers:
		return r.limits.MaxHeadersSize, true
	default:
		return 0, false
	}
}

// readVarint reads one varint. It returns io.EOF only if the stream ends
// before the first byte of the varint.
func (r *Reader) readVarint() (uint64, error) {
	first, err := r.br.Peek(1)
	if err != nil {
		return 0, err
	}
	n := 1 << (first[0] >> 6)
	buf, err := r.br.Peek(n)
	if err != nil {
		return 0, unexpectedEOF(err)
	}
	v, _, err := quicvarint.Parse(buf)
	if err != nil {
		return 0, err
	}
	if quicvarint.Len(v) != n {
		return 0, fmt.Errorf("%w: %d encoded in %d bytes", ErrNonMinimal, v, n)
	}
	if _, err := r.br.Discard(n); err != nil {
		return 0, err
	}
	return v, nil
}

func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// AppendFrame appends a frame with the given type and payload to b.
func AppendFrame(b []byte, t Type, payload []byte) []byte {
	b = appendHeader(b, t, len(payload))
	return append(b, payload...)
}

// AppendProto appends a frame whose payload is m in protobuf wire format,
// marshaling m straight into b.
func AppendProto(b []byte, t Type, m proto.Message) ([]byte, error) {
	size := proto.Size(m)
	b = appendHeader(b, t, size)
	start := len(b)
	b, err := proto.MarshalOptions{UseCachedSize: true}.MarshalAppend(b, m)
	if err != nil {
		return nil, fmt.Errorf("frame: marshal %v payload: %w", t, err)
	}
	// A mismatch would corrupt the stream for the peer; it means m was
	// modified concurrently with this call.
	if got := len(b) - start; got != size {
		return nil, fmt.Errorf("frame: %v payload is %d bytes, header says %d", t, got, size)
	}
	return b, nil
}

func appendHeader(b []byte, t Type, payloadLen int) []byte {
	b = quicvarint.Append(b, uint64(t))
	return quicvarint.Append(b, uint64(payloadLen)) //nolint:gosec // payloadLen comes from len() or proto.Size, never negative
}
