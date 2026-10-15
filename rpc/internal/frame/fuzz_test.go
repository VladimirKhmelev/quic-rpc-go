package frame_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/quic-go/quic-go/quicvarint"
	"google.golang.org/protobuf/proto"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame/framepb"
)

// FuzzReadFrame checks that the reader never panics, reports only known
// errors and that every frame it accepts re-encodes to exactly the bytes
// it was read from.
func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{})
	f.Add(frame.AppendFrame(nil, frame.TypeMessage, []byte("hello")))
	f.Add(frame.AppendFrame(frame.AppendFrame(nil, frame.TypeHeaders, pattern(70)), frame.TypeTrailers, nil))
	f.Add(quicvarint.AppendWithLen(nil, 0, 2))
	f.Add([]byte{0x00, 0x80, 0x01, 0x00, 0x00})
	f.Add(frame.AppendFrame(nil, 0x07, nil))

	limits := frame.Limits{MaxMessageSize: 1 << 16, MaxHeadersSize: 1 << 10}
	f.Fuzz(func(t *testing.T, data []byte) {
		r := frame.NewReader(bytes.NewReader(data), limits)
		var reencoded []byte
		for {
			fr, err := r.ReadFrame()
			if err != nil {
				if !isReadError(err) {
					t.Fatalf("unexpected error: %v", err)
				}
				break
			}
			reencoded = frame.AppendFrame(reencoded, fr.Type, fr.Payload)
		}
		if !bytes.HasPrefix(data, reencoded) {
			t.Fatalf("re-encoded frames %x are not a prefix of the input %x", reencoded, data)
		}
	})
}

func isReadError(err error) bool {
	for _, known := range []error{io.EOF, io.ErrUnexpectedEOF, frame.ErrUnknownType, frame.ErrTooLarge, frame.ErrNonMinimal} {
		if errors.Is(err, known) {
			return true
		}
	}
	return false
}

func FuzzDecodeHeaders(f *testing.F) {
	for _, h := range []*framepb.Headers{
		{},
		{Method: "/chat.v1.Chat/Send", TimeoutUs: 1000, Metadata: []*framepb.Metadata{md("authorization", "Bearer x")}},
		{Metadata: []*framepb.Metadata{md("trace-bin", "\x00\x01"), md("k", "a", "b")}},
	} {
		f.Add(mustMarshal(h))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := frame.DecodeHeaders(data)
		if err != nil {
			if !errors.Is(err, frame.ErrMalformed) {
				t.Fatalf("unexpected error: %v", err)
			}
			return
		}
		again, err := frame.DecodeHeaders(mustMarshal(h))
		if err != nil {
			t.Fatalf("re-encoded headers rejected: %v", err)
		}
		if !proto.Equal(h, again) {
			t.Fatalf("round trip changed headers: %v != %v", h, again)
		}
	})
}

func FuzzDecodeTrailers(f *testing.F) {
	for _, tr := range []*framepb.Trailers{
		{},
		{Code: 5, Message: "not found", Metadata: []*framepb.Metadata{md("k", "v")}},
	} {
		f.Add(mustMarshal(tr))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		tr, err := frame.DecodeTrailers(data)
		if err != nil {
			if !errors.Is(err, frame.ErrMalformed) {
				t.Fatalf("unexpected error: %v", err)
			}
			return
		}
		again, err := frame.DecodeTrailers(mustMarshal(tr))
		if err != nil {
			t.Fatalf("re-encoded trailers rejected: %v", err)
		}
		if !proto.Equal(tr, again) {
			t.Fatalf("round trip changed trailers: %v != %v", tr, again)
		}
	})
}

func mustMarshal(m proto.Message) []byte {
	b, err := proto.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}
