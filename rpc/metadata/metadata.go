// Package metadata carries call metadata: key-value pairs sent at the start
// of a call (headers) and at its end (trailers). The API mirrors grpc-go's
// metadata package.
//
// Keys consist of [0-9a-z-_.] and are lowercased by the functions of this
// package. Values of keys ending in "-bin" may hold arbitrary bytes; other
// values must be printable ASCII. Metadata that breaks these rules fails the
// call with codes.Internal when it is sent.
package metadata

import (
	"context"
	"fmt"
	"strings"
)

// MD maps lowercase keys to their values.
type MD map[string][]string

// New returns metadata with one value per key of m.
func New(m map[string]string) MD {
	md := make(MD, len(m))
	for k, v := range m {
		k = strings.ToLower(k)
		md[k] = append(md[k], v)
	}
	return md
}

// Pairs returns metadata from alternating keys and values. A key may repeat
// to carry several values. Pairs panics if len(kv) is odd.
func Pairs(kv ...string) MD {
	if len(kv)%2 == 1 {
		panic(fmt.Sprintf("metadata: Pairs got an odd number of arguments: %d", len(kv)))
	}
	md := make(MD, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k := strings.ToLower(kv[i])
		md[k] = append(md[k], kv[i+1])
	}
	return md
}

// String reports only how many keys md has. Keys and values are hidden,
// because metadata often carries credentials and ends up in logs; print
// map[string][]string(md) to see everything.
func (md MD) String() string {
	return fmt.Sprintf("map[<%d redacted>]", len(md))
}

// Len returns the number of keys.
func (md MD) Len() int { return len(md) }

// Copy returns a deep copy of md.
func (md MD) Copy() MD {
	out := make(MD, len(md))
	for k, v := range md {
		out[k] = copyOf(v)
	}
	return out
}

// Get returns the values of key k, which is lowercased first.
func (md MD) Get(k string) []string {
	return md[strings.ToLower(k)]
}

// Set replaces the values of key k, which is lowercased first. Setting no
// values is a no-op.
func (md MD) Set(k string, vals ...string) {
	if len(vals) == 0 {
		return
	}
	md[strings.ToLower(k)] = vals
}

// Append adds values to key k, which is lowercased first.
func (md MD) Append(k string, vals ...string) {
	if len(vals) == 0 {
		return
	}
	k = strings.ToLower(k)
	md[k] = append(md[k], vals...)
}

// Delete removes key k, which is lowercased first.
func (md MD) Delete(k string) {
	delete(md, strings.ToLower(k))
}

// Join merges several metadata into a new one; values of the same key are
// concatenated in argument order.
func Join(mds ...MD) MD {
	out := MD{}
	for _, md := range mds {
		for k, v := range md {
			out[k] = append(out[k], v...)
		}
	}
	return out
}

type (
	incomingKey struct{}
	outgoingKey struct{}
)

// outgoing is the metadata set by NewOutgoingContext plus the pairs added
// later by AppendToOutgoingContext, kept apart so that appending does not
// copy the map.
type outgoing struct {
	md    MD
	added [][]string
}

// NewIncomingContext returns a context carrying md as metadata received from
// the peer. The call layer uses it on the server side.
func NewIncomingContext(ctx context.Context, md MD) context.Context {
	return context.WithValue(ctx, incomingKey{}, md)
}

// NewOutgoingContext returns a context whose calls send md, replacing any
// outgoing metadata of ctx.
func NewOutgoingContext(ctx context.Context, md MD) context.Context {
	return context.WithValue(ctx, outgoingKey{}, outgoing{md: md})
}

// AppendToOutgoingContext returns a context whose calls also send the given
// alternating keys and values. It panics if len(kv) is odd.
func AppendToOutgoingContext(ctx context.Context, kv ...string) context.Context {
	if len(kv)%2 == 1 {
		panic(fmt.Sprintf("metadata: AppendToOutgoingContext got an odd number of arguments: %d", len(kv)))
	}
	prev, _ := ctx.Value(outgoingKey{}).(outgoing)
	added := make([][]string, len(prev.added), len(prev.added)+1)
	copy(added, prev.added)
	pairs := make([]string, len(kv))
	for i := 0; i < len(kv); i += 2 {
		pairs[i], pairs[i+1] = strings.ToLower(kv[i]), kv[i+1]
	}
	added = append(added, pairs)
	return context.WithValue(ctx, outgoingKey{}, outgoing{md: prev.md, added: added})
}

// FromIncomingContext returns a copy of the metadata received from the peer.
func FromIncomingContext(ctx context.Context) (MD, bool) {
	md, ok := ctx.Value(incomingKey{}).(MD)
	if !ok {
		return nil, false
	}
	out := make(MD, len(md))
	for k, v := range md {
		out[strings.ToLower(k)] = copyOf(v)
	}
	return out, true
}

// ValueFromIncomingContext returns a copy of the values of key received
// from the peer; the key is matched case-insensitively.
func ValueFromIncomingContext(ctx context.Context, key string) []string {
	md, ok := ctx.Value(incomingKey{}).(MD)
	if !ok {
		return nil
	}
	if v, ok := md[key]; ok {
		return copyOf(v)
	}
	for k, v := range md {
		if strings.EqualFold(k, key) {
			return copyOf(v)
		}
	}
	return nil
}

// FromOutgoingContext returns a copy of the metadata calls made with ctx
// will send.
func FromOutgoingContext(ctx context.Context) (MD, bool) {
	o, ok := ctx.Value(outgoingKey{}).(outgoing)
	if !ok {
		return nil, false
	}
	size := len(o.md)
	for _, pairs := range o.added {
		size += len(pairs) / 2
	}
	out := make(MD, size)
	for k, v := range o.md {
		k = strings.ToLower(k)
		out[k] = append(out[k], v...)
	}
	for _, pairs := range o.added {
		for i := 0; i < len(pairs); i += 2 {
			out[pairs[i]] = append(out[pairs[i]], pairs[i+1])
		}
	}
	return out, true
}

func copyOf(v []string) []string {
	out := make([]string, len(v))
	copy(out, v)
	return out
}
