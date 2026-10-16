// Package call is the wire side of a call shared by the client and the
// server: it builds frames, reads them in the order docs/protocol.md allows
// and maps transport and protocol errors to statuses.
package call

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame/framepb"
	istatus "github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/status"
)

// TimeoutMicros converts the time left until a deadline to the value of
// Headers.timeout_us. It rounds up, so a positive remainder never turns into
// 0, which means no deadline; ok is false if no time is left.
func TimeoutMicros(left time.Duration) (us uint64, ok bool) {
	if left <= 0 {
		return 0, false
	}
	return uint64((left + time.Microsecond - 1) / time.Microsecond), true
}

// AppendRequestHeaders appends the HEADERS frame that opens a call.
func AppendRequestHeaders(b []byte, method string, timeoutUs uint64, md map[string][]string) ([]byte, error) {
	h := &framepb.Headers{Method: method, TimeoutUs: timeoutUs, Metadata: frame.EncodeMetadata(md)}
	if err := frame.ValidateMetadata(h.Metadata); err != nil {
		return nil, fmt.Errorf("invalid metadata: %w", err)
	}
	return frame.AppendProto(b, frame.TypeHeaders, h)
}

// AppendResponseHeaders appends the server's HEADERS frame.
func AppendResponseHeaders(b []byte, md map[string][]string) ([]byte, error) {
	h := &framepb.Headers{Metadata: frame.EncodeMetadata(md)}
	if err := frame.ValidateMetadata(h.Metadata); err != nil {
		return nil, fmt.Errorf("invalid header metadata: %w", err)
	}
	return frame.AppendProto(b, frame.TypeHeaders, h)
}

// AppendMessage appends a MESSAGE frame; m must be a protobuf message.
func AppendMessage(b []byte, m any) ([]byte, error) {
	pm, ok := m.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("message of type %T is not a protobuf message", m)
	}
	return frame.AppendProto(b, frame.TypeMessage, pm)
}

// Unmarshal decodes a MESSAGE payload into m, which must be a protobuf message.
func Unmarshal(payload []byte, m any) error {
	pm, ok := m.(proto.Message)
	if !ok {
		return fmt.Errorf("message of type %T is not a protobuf message", m)
	}
	return proto.Unmarshal(payload, pm)
}

// AppendTrailers appends the TRAILERS frame that ends a call. A message that
// is not valid UTF-8 is repaired, since protobuf strings must be UTF-8.
func AppendTrailers(b []byte, st *istatus.Status, md map[string][]string) ([]byte, error) {
	code, msg, details := istatus.Wire(st)
	t := &framepb.Trailers{
		Code:     uint32(code),
		Message:  strings.ToValidUTF8(msg, string(utf8.RuneError)),
		Details:  details,
		Metadata: frame.EncodeMetadata(md),
	}
	if err := frame.ValidateMetadata(t.Metadata); err != nil {
		return nil, fmt.Errorf("invalid trailer metadata: %w", err)
	}
	return frame.AppendProto(b, frame.TypeTrailers, t)
}
