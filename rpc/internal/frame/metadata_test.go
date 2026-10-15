package frame_test

import (
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame/framepb"
)

func TestEncodeMetadata(t *testing.T) {
	md := map[string][]string{
		"x-ids":     {"1", "2"},
		"auth":      {"Bearer x"},
		"empty":     {},
		"trace-bin": {"\x00\xff"},
	}
	got := frame.EncodeMetadata(md)
	want := []*framepb.Metadata{
		{Key: "auth", Values: [][]byte{[]byte("Bearer x")}},
		{Key: "trace-bin", Values: [][]byte{{0x00, 0xff}}},
		{Key: "x-ids", Values: [][]byte{[]byte("1"), []byte("2")}},
	}
	if len(got) != len(want) {
		t.Fatalf("EncodeMetadata returned %d entries, want %d (sorted, without empty keys)", len(got), len(want))
	}
	for i := range want {
		if !proto.Equal(got[i], want[i]) {
			t.Errorf("entry %d = %v, want %v", i, got[i], want[i])
		}
	}
	if err := frame.ValidateMetadata(got); err != nil {
		t.Errorf("encoded valid metadata fails validation: %v", err)
	}

	back := frame.DecodeMetadata(got)
	delete(md, "empty")
	if !reflect.DeepEqual(back, md) {
		t.Errorf("DecodeMetadata(EncodeMetadata(md)) = %v, want %v", back, md)
	}

	if got := frame.EncodeMetadata(nil); got != nil {
		t.Errorf("EncodeMetadata(nil) = %v, want nil", got)
	}
}

func TestEncodedInvalidMetadataFailsValidation(t *testing.T) {
	for _, md := range []map[string][]string{
		{"Upper": {"v"}},
		{"k": {"line\nbreak"}},
	} {
		if err := frame.ValidateMetadata(frame.EncodeMetadata(md)); err == nil {
			t.Errorf("metadata %v passed validation", md)
		}
	}
}
