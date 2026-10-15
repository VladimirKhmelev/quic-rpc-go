package frame

import (
	"slices"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame/framepb"
)

// EncodeMetadata converts metadata to its wire form. Keys are sorted, so the
// encoding is deterministic, and keys without values are skipped, as grpc-go
// does. The result is not validated: call ValidateMetadata before sending.
func EncodeMetadata(md map[string][]string) []*framepb.Metadata {
	if len(md) == 0 {
		return nil
	}
	keys := make([]string, 0, len(md))
	for k, v := range md {
		if len(v) > 0 {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	out := make([]*framepb.Metadata, len(keys))
	for i, k := range keys {
		values := make([][]byte, len(md[k]))
		for j, v := range md[k] {
			values[j] = []byte(v)
		}
		out[i] = &framepb.Metadata{Key: k, Values: values}
	}
	return out
}

// DecodeMetadata converts validated wire metadata back to a map.
func DecodeMetadata(md []*framepb.Metadata) map[string][]string {
	out := make(map[string][]string, len(md))
	for _, m := range md {
		values := make([]string, len(m.GetValues()))
		for i, v := range m.GetValues() {
			values[i] = string(v)
		}
		out[m.GetKey()] = values
	}
	return out
}
