package frame_test

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/iotest"

	"github.com/quic-go/quic-go/quicvarint"
	"google.golang.org/protobuf/proto"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame/framepb"
)

func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		typ  frame.Type
		size int
	}{
		{"empty message", frame.TypeMessage, 0},
		{"largest 1-byte length", frame.TypeMessage, 63},
		{"smallest 2-byte length", frame.TypeMessage, 64},
		{"largest 2-byte length", frame.TypeMessage, 16383},
		{"smallest 4-byte length", frame.TypeMessage, 16384},
		{"message at default limit", frame.TypeMessage, frame.DefaultMaxMessageSize},
		{"headers at default limit", frame.TypeHeaders, frame.DefaultMaxHeadersSize},
		{"trailers", frame.TypeTrailers, 10},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := pattern(tt.size)
			wire := frame.AppendFrame(nil, tt.typ, payload)
			wantLen := quicvarint.Len(uint64(tt.typ)) + quicvarint.Len(uint64(tt.size)) + tt.size
			if len(wire) != wantLen {
				t.Fatalf("encoded length = %d, want %d", len(wire), wantLen)
			}

			r := frame.NewReader(bytes.NewReader(wire), frame.DefaultLimits())
			got, err := r.ReadFrame()
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if got.Type != tt.typ || !bytes.Equal(got.Payload, payload) {
				t.Fatalf("got %v with %d bytes, want %v with %d bytes", got.Type, len(got.Payload), tt.typ, len(payload))
			}
			if _, err := r.ReadFrame(); !errors.Is(err, io.EOF) {
				t.Fatalf("ReadFrame after last frame: %v, want io.EOF", err)
			}
		})
	}
}

func TestReadSequence(t *testing.T) {
	frames := []frame.Frame{
		{Type: frame.TypeHeaders, Payload: pattern(30)},
		{Type: frame.TypeMessage, Payload: pattern(0)},
		{Type: frame.TypeMessage, Payload: pattern(20000)},
		{Type: frame.TypeTrailers, Payload: pattern(5)},
	}
	var wire []byte
	for _, f := range frames {
		wire = frame.AppendFrame(wire, f.Type, f.Payload)
	}

	readers := map[string]func(io.Reader) io.Reader{
		"whole":    func(r io.Reader) io.Reader { return r },
		"one byte": iotest.OneByteReader,
		"half":     iotest.HalfReader,
	}
	for name, wrap := range readers {
		t.Run(name, func(t *testing.T) {
			r := frame.NewReader(wrap(bytes.NewReader(wire)), frame.DefaultLimits())
			for i, want := range frames {
				got, err := r.ReadFrame()
				if err != nil {
					t.Fatalf("frame %d: %v", i, err)
				}
				if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
					t.Fatalf("frame %d: got %v with %d bytes, want %v with %d bytes",
						i, got.Type, len(got.Payload), want.Type, len(want.Payload))
				}
			}
			if _, err := r.ReadFrame(); !errors.Is(err, io.EOF) {
				t.Fatalf("after last frame: %v, want io.EOF", err)
			}
		})
	}
}

func TestTruncated(t *testing.T) {
	// 100-byte payload: 1-byte type, 2-byte length.
	wire := frame.AppendFrame(nil, frame.TypeMessage, pattern(100))
	for cut := range len(wire) {
		r := frame.NewReader(bytes.NewReader(wire[:cut]), frame.DefaultLimits())
		_, err := r.ReadFrame()
		want := io.ErrUnexpectedEOF
		if cut == 0 {
			want = io.EOF
		}
		if !errors.Is(err, want) {
			t.Errorf("cut at %d: got %v, want %v", cut, err, want)
		}
	}
}

func TestUnknownType(t *testing.T) {
	for _, typ := range []frame.Type{0x04, 0x3f, 0x40, 1 << 20} {
		wire := frame.AppendFrame(nil, typ, pattern(3))
		_, err := frame.NewReader(bytes.NewReader(wire), frame.DefaultLimits()).ReadFrame()
		if !errors.Is(err, frame.ErrUnknownType) {
			t.Errorf("type %v: got %v, want ErrUnknownType", typ, err)
		}
	}
}

// TestGoAway: GOAWAY has an empty payload.
func TestGoAway(t *testing.T) {
	f, err := frame.NewReader(bytes.NewReader(frame.AppendFrame(nil, frame.TypeGoAway, nil)), frame.DefaultLimits()).ReadFrame()
	if err != nil || f.Type != frame.TypeGoAway || len(f.Payload) != 0 {
		t.Fatalf("ReadFrame: %v %q, %v", f.Type, f.Payload, err)
	}
	wire := frame.AppendFrame(nil, frame.TypeGoAway, []byte{0})
	if _, err := frame.NewReader(bytes.NewReader(wire), frame.DefaultLimits()).ReadFrame(); !errors.Is(err, frame.ErrTooLarge) {
		t.Fatalf("GOAWAY with a payload: %v, want ErrTooLarge", err)
	}
}

func TestNonMinimalVarint(t *testing.T) {
	tests := map[string][]byte{
		"type in 2 bytes":   append(quicvarint.AppendWithLen(nil, uint64(frame.TypeMessage), 2), 0x01, 0xff),
		"type in 8 bytes":   append(quicvarint.AppendWithLen(nil, uint64(frame.TypeHeaders), 8), 0x00),
		"length in 2 bytes": append(quicvarint.AppendWithLen([]byte{0x00}, 1, 2), 0xff),
		"length in 4 bytes": append(quicvarint.AppendWithLen([]byte{0x00}, 64, 4), pattern(64)...),
	}
	for name, wire := range tests {
		_, err := frame.NewReader(bytes.NewReader(wire), frame.DefaultLimits()).ReadFrame()
		if !errors.Is(err, frame.ErrNonMinimal) {
			t.Errorf("%s: got %v, want ErrNonMinimal", name, err)
		}
	}
}

func TestTooLarge(t *testing.T) {
	limits := frame.Limits{MaxMessageSize: 100, MaxHeadersSize: 10}
	tests := []struct {
		name string
		typ  frame.Type
		size uint64
	}{
		{"message over limit", frame.TypeMessage, 101},
		{"headers over limit", frame.TypeHeaders, 11},
		{"trailers over limit", frame.TypeTrailers, 11},
		{"largest varint", frame.TypeMessage, quicvarint.Max},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Only the frame header: the limit must be checked before the
			// payload is read or allocated.
			wire := quicvarint.Append(quicvarint.Append(nil, uint64(tt.typ)), tt.size)
			_, err := frame.NewReader(bytes.NewReader(wire), limits).ReadFrame()
			if !errors.Is(err, frame.ErrTooLarge) {
				t.Fatalf("got %v, want ErrTooLarge", err)
			}
			tl, ok := errors.AsType[*frame.TooLargeError](err)
			if !ok || tl.Type != tt.typ || tl.Size != tt.size {
				t.Fatalf("got %#v, want a TooLargeError for %v of %d bytes", err, tt.typ, tt.size)
			}
		})
	}
}

func TestReaderErrorIsPassedThrough(t *testing.T) {
	errStream := errors.New("stream reset")
	header := frame.AppendFrame(nil, frame.TypeMessage, pattern(10))[:3]
	tests := map[string]io.Reader{
		"at frame boundary": iotest.ErrReader(errStream),
		"inside frame":      io.MultiReader(bytes.NewReader(header), iotest.ErrReader(errStream)),
	}
	for name, src := range tests {
		_, err := frame.NewReader(src, frame.DefaultLimits()).ReadFrame()
		if !errors.Is(err, errStream) {
			t.Errorf("%s: got %v, want the reader's error", name, err)
		}
	}
}

func TestAppendProto(t *testing.T) {
	h := &framepb.Headers{
		Method:    "/chat.v1.Chat/Send",
		TimeoutUs: 1500000,
		Metadata:  []*framepb.Metadata{{Key: "authorization", Values: [][]byte{[]byte("Bearer x")}}},
	}
	prefix := []byte("prefix")
	got, err := frame.AppendProto(prefix, frame.TypeHeaders, h)
	if err != nil {
		t.Fatalf("AppendProto: %v", err)
	}
	payload, err := proto.Marshal(h)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if want := frame.AppendFrame([]byte("prefix"), frame.TypeHeaders, payload); !bytes.Equal(got, want) {
		t.Fatalf("AppendProto = %x, want %x", got, want)
	}

	f, err := frame.NewReader(bytes.NewReader(got[len(prefix):]), frame.DefaultLimits()).ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	decoded, err := frame.DecodeHeaders(f.Payload)
	if err != nil {
		t.Fatalf("DecodeHeaders: %v", err)
	}
	if !proto.Equal(decoded, h) {
		t.Fatalf("decoded %v, want %v", decoded, h)
	}
}

func TestAppendProtoMarshalError(t *testing.T) {
	invalid := &framepb.Headers{Method: "/s/\xff"}
	if _, err := frame.AppendProto(nil, frame.TypeHeaders, invalid); err == nil {
		t.Fatal("AppendProto accepted a string field with invalid UTF-8")
	}
}

func TestTypeString(t *testing.T) {
	tests := map[frame.Type]string{
		frame.TypeMessage:  "MESSAGE",
		frame.TypeHeaders:  "HEADERS",
		frame.TypeTrailers: "TRAILERS",
		0x2a:               "Type(0x2a)",
	}
	for typ, want := range tests {
		if got := typ.String(); got != want {
			t.Errorf("Type(%d).String() = %q, want %q", uint64(typ), got, want)
		}
	}
}
