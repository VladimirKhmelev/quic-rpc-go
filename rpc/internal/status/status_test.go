package status_test

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	istatus "github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/status"
)

// The wire format rejects codes above frame.MaxStatusCode; it must stay the
// largest code.
func TestMaxStatusCodeMatchesCodes(t *testing.T) {
	if frame.MaxStatusCode != uint32(codes.Unauthenticated) {
		t.Fatalf("frame.MaxStatusCode = %d, codes.Unauthenticated = %d", frame.MaxStatusCode, codes.Unauthenticated)
	}
}

func TestWireRoundTripKeepsUnknownDetails(t *testing.T) {
	unknown := &anypb.Any{TypeUrl: "type.googleapis.com/not.linked.Detail", Value: []byte{0x08, 0x01}}
	s := istatus.FromWire(codes.Aborted, "conflict", []*anypb.Any{unknown})

	c, msg, details := istatus.Wire(s)
	if c != codes.Aborted || msg != "conflict" || len(details) != 1 || !proto.Equal(details[0], unknown) {
		t.Fatalf("Wire = %v, %q, %v; want the fields passed to FromWire", c, msg, details)
	}
	if got := s.Details(); len(got) != 1 {
		t.Fatalf("Details() = %v, want one entry", got)
	} else if _, isErr := got[0].(error); !isErr {
		t.Fatalf("Details()[0] = %T, want the unpacking error for an unknown type", got[0])
	}
}

func TestWireOfNil(t *testing.T) {
	c, msg, details := istatus.Wire(nil)
	if c != codes.OK || msg != "" || details != nil {
		t.Fatalf("Wire(nil) = %v, %q, %v; want OK and empty fields", c, msg, details)
	}
}
