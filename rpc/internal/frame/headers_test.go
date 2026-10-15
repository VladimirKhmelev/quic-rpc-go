package frame_test

import (
	"errors"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame/framepb"
)

func md(key string, values ...string) *framepb.Metadata {
	m := &framepb.Metadata{Key: key}
	for _, v := range values {
		m.Values = append(m.Values, []byte(v))
	}
	return m
}

func marshal(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.Marshal(m)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return b
}

// withUnknownField appends field 15, which no message in the schema uses.
func withUnknownField(b []byte) []byte {
	b = protowire.AppendTag(b, 15, protowire.VarintType)
	return protowire.AppendVarint(b, 1)
}

func TestDecodeHeaders(t *testing.T) {
	valid := []struct {
		name string
		h    *framepb.Headers
	}{
		{"request", &framepb.Headers{
			Method:    "/chat.v1.Chat/Send",
			TimeoutUs: 250000,
			Metadata:  []*framepb.Metadata{md("authorization", "Bearer abc"), md("x-ids", "1", "2")},
		}},
		{"response headers", &framepb.Headers{Metadata: []*framepb.Metadata{md("set-cookie", "a=b")}}},
		{"empty", &framepb.Headers{}},
		{"largest timeout", &framepb.Headers{Method: "/s/m", TimeoutUs: frame.MaxTimeoutMicros}},
		{"key charset", &framepb.Headers{Metadata: []*framepb.Metadata{md("a-z_0.9", "v")}}},
		{"printable edges", &framepb.Headers{Metadata: []*framepb.Metadata{md("k", " ~")}}},
		{"binary value", &framepb.Headers{Metadata: []*framepb.Metadata{md("trace-bin", "\x00\xff\n")}}},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			got, err := frame.DecodeHeaders(marshal(t, tt.h))
			if err != nil {
				t.Fatalf("DecodeHeaders: %v", err)
			}
			if !proto.Equal(got, tt.h) {
				t.Fatalf("got %v, want %v", got, tt.h)
			}
		})
	}

	metadataBytes := marshal(t, md("k", "v"))
	nestedUnknown := protowire.AppendTag(nil, 3, protowire.BytesType)
	nestedUnknown = protowire.AppendBytes(nestedUnknown, withUnknownField(metadataBytes))

	invalid := []struct {
		name    string
		payload []byte
	}{
		{"not protobuf", []byte{0xff, 0xff, 0xff}},
		{"method without leading slash", marshal(t, &framepb.Headers{Method: "s/m"})},
		{"method without name", marshal(t, &framepb.Headers{Method: "/s/"})},
		{"method without service", marshal(t, &framepb.Headers{Method: "//m"})},
		{"method with extra slash", marshal(t, &framepb.Headers{Method: "/s/m/x"})},
		{"method without separator", marshal(t, &framepb.Headers{Method: "/s"})},
		{"method not utf-8", marshalInvalidUTF8Method()},
		{"timeout overflow", marshal(t, &framepb.Headers{TimeoutUs: frame.MaxTimeoutMicros + 1})},
		{"empty key", marshal(t, &framepb.Headers{Metadata: []*framepb.Metadata{md("", "v")}})},
		{"uppercase key", marshal(t, &framepb.Headers{Metadata: []*framepb.Metadata{md("Authorization", "v")}})},
		{"key with space", marshal(t, &framepb.Headers{Metadata: []*framepb.Metadata{md("a b", "v")}})},
		{"pseudo-header key", marshal(t, &framepb.Headers{Metadata: []*framepb.Metadata{md(":path", "v")}})},
		{"duplicate key", marshal(t, &framepb.Headers{Metadata: []*framepb.Metadata{md("k", "1"), md("k", "2")}})},
		{"key without values", marshal(t, &framepb.Headers{Metadata: []*framepb.Metadata{md("k")}})},
		{"newline in value", marshal(t, &framepb.Headers{Metadata: []*framepb.Metadata{md("k", "a\nb")}})},
		{"DEL in value", marshal(t, &framepb.Headers{Metadata: []*framepb.Metadata{md("k", "\x7f")}})},
		{"unknown field", withUnknownField(marshal(t, &framepb.Headers{Method: "/s/m"}))},
		{"unknown nested field", nestedUnknown},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := frame.DecodeHeaders(tt.payload); !errors.Is(err, frame.ErrMalformed) {
				t.Fatalf("got %v, want ErrMalformed", err)
			}
		})
	}
}

// marshalInvalidUTF8Method encodes field 1 (method) by hand, since
// proto.Marshal refuses invalid UTF-8 in string fields.
func marshalInvalidUTF8Method() []byte {
	b := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(b, []byte("/s/\xff"))
}

func TestDecodeTrailers(t *testing.T) {
	detail, err := anypb.New(md("reason", "quota"))
	if err != nil {
		t.Fatalf("anypb.New: %v", err)
	}

	valid := []struct {
		name string
		tr   *framepb.Trailers
	}{
		{"ok", &framepb.Trailers{}},
		{"error with details", &framepb.Trailers{
			Code:     5,
			Message:  "chat not found",
			Details:  []*anypb.Any{detail},
			Metadata: []*framepb.Metadata{md("retry-after", "3")},
		}},
		{"largest code", &framepb.Trailers{Code: frame.MaxStatusCode}},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			got, err := frame.DecodeTrailers(marshal(t, tt.tr))
			if err != nil {
				t.Fatalf("DecodeTrailers: %v", err)
			}
			if !proto.Equal(got, tt.tr) {
				t.Fatalf("got %v, want %v", got, tt.tr)
			}
		})
	}

	invalid := []struct {
		name    string
		payload []byte
	}{
		{"not protobuf", []byte{0x0a}},
		{"unknown code", marshal(t, &framepb.Trailers{Code: frame.MaxStatusCode + 1})},
		{"detail without type URL", marshal(t, &framepb.Trailers{Code: 13, Details: []*anypb.Any{{Value: []byte{1}}}})},
		{"invalid metadata", marshal(t, &framepb.Trailers{Metadata: []*framepb.Metadata{md("K", "v")}})},
		{"unknown field", withUnknownField(marshal(t, &framepb.Trailers{Code: 2}))},
	}
	for _, tt := range invalid {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := frame.DecodeTrailers(tt.payload); !errors.Is(err, frame.ErrMalformed) {
				t.Fatalf("got %v, want ErrMalformed", err)
			}
		})
	}
}
