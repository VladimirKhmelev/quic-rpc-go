package frame

import (
	"fmt"
	"math"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame/framepb"
)

// MaxStatusCode is the largest status code, Unauthenticated in gRPC numbering.
const MaxStatusCode = 16

// MaxTimeoutMicros is the largest timeout that still fits time.Duration.
const MaxTimeoutMicros = math.MaxInt64 / 1000

// DecodeHeaders parses and validates a HEADERS payload. Whether method and
// timeout are allowed depends on the direction and is checked by the caller.
func DecodeHeaders(payload []byte) (*framepb.Headers, error) {
	h := new(framepb.Headers)
	if err := unmarshalStrict(payload, h); err != nil {
		return nil, fmt.Errorf("%w: headers: %w", ErrMalformed, err)
	}
	if err := validateMethod(h.GetMethod()); err != nil {
		return nil, fmt.Errorf("%w: headers: %w", ErrMalformed, err)
	}
	if h.GetTimeoutUs() > MaxTimeoutMicros {
		return nil, fmt.Errorf("%w: headers: timeout %dus overflows time.Duration", ErrMalformed, h.GetTimeoutUs())
	}
	if err := ValidateMetadata(h.GetMetadata()); err != nil {
		return nil, fmt.Errorf("%w: headers: %w", ErrMalformed, err)
	}
	return h, nil
}

// DecodeTrailers parses and validates a TRAILERS payload.
func DecodeTrailers(payload []byte) (*framepb.Trailers, error) {
	t := new(framepb.Trailers)
	if err := unmarshalStrict(payload, t); err != nil {
		return nil, fmt.Errorf("%w: trailers: %w", ErrMalformed, err)
	}
	if t.GetCode() > MaxStatusCode {
		return nil, fmt.Errorf("%w: trailers: unknown status code %d", ErrMalformed, t.GetCode())
	}
	for _, d := range t.GetDetails() {
		if d.GetTypeUrl() == "" {
			return nil, fmt.Errorf("%w: trailers: status detail without type URL", ErrMalformed)
		}
	}
	if err := ValidateMetadata(t.GetMetadata()); err != nil {
		return nil, fmt.Errorf("%w: trailers: %w", ErrMalformed, err)
	}
	return t, nil
}

// ValidateMetadata applies grpc-go's rules to metadata: keys are non-empty,
// unique and consist of [0-9a-z-_.]; every key has at least one value;
// values of keys without the "-bin" suffix are printable ASCII.
func ValidateMetadata(md []*framepb.Metadata) error {
	seen := make(map[string]struct{}, len(md))
	for _, m := range md {
		key := m.GetKey()
		if err := validateKey(key); err != nil {
			return err
		}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("duplicate metadata key %q", key)
		}
		seen[key] = struct{}{}
		if len(m.GetValues()) == 0 {
			return fmt.Errorf("metadata key %q has no values", key)
		}
		if strings.HasSuffix(key, "-bin") {
			continue
		}
		for _, v := range m.GetValues() {
			if err := validateValue(v); err != nil {
				return fmt.Errorf("metadata key %q: %w", key, err)
			}
		}
	}
	return nil
}

func validateKey(key string) error {
	if key == "" {
		return fmt.Errorf("empty metadata key")
	}
	for i := range len(key) {
		c := key[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return fmt.Errorf("metadata key %q contains characters outside [0-9a-z-_.]", key)
		}
	}
	return nil
}

func validateValue(v []byte) error {
	for _, c := range v {
		if c < 0x20 || c > 0x7e {
			return fmt.Errorf("value contains non-printable byte %#x", c)
		}
	}
	return nil
}

// validateMethod accepts an empty method or "/service/method" with both
// parts non-empty.
func validateMethod(method string) error {
	if method == "" {
		return nil
	}
	service, name, ok := strings.Cut(strings.TrimPrefix(method, "/"), "/")
	if !strings.HasPrefix(method, "/") || !ok || service == "" || name == "" || strings.Contains(name, "/") {
		return fmt.Errorf("method %q is not of the form /service/method", method)
	}
	return nil
}

// unmarshalStrict unmarshals payload into m and rejects fields that are not
// part of the schema, at any nesting level.
func unmarshalStrict(payload []byte, m proto.Message) error {
	if err := proto.Unmarshal(payload, m); err != nil {
		return err
	}
	if hasUnknownFields(m.ProtoReflect()) {
		return fmt.Errorf("unknown fields")
	}
	return nil
}

func hasUnknownFields(m protoreflect.Message) bool {
	if len(m.GetUnknown()) > 0 {
		return true
	}
	found := false
	m.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Message() == nil {
			return true
		}
		if fd.IsList() {
			list := v.List()
			for i := range list.Len() {
				if hasUnknownFields(list.Get(i).Message()) {
					found = true
					return false
				}
			}
			return true
		}
		found = hasUnknownFields(v.Message())
		return !found
	})
	return found
}
