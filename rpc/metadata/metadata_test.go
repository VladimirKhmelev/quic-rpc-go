package metadata_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
)

func TestNewAndPairs(t *testing.T) {
	if got, want := metadata.New(map[string]string{"Authorization": "Bearer x"}), (metadata.MD{"authorization": {"Bearer x"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("New = %v, want %v", map[string][]string(got), map[string][]string(want))
	}
	got := metadata.Pairs("X-Ids", "1", "x-ids", "2", "k", "v")
	want := metadata.MD{"x-ids": {"1", "2"}, "k": {"v"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Pairs = %v, want %v", map[string][]string(got), map[string][]string(want))
	}
	odd := []string{"k"}
	mustPanic(t, "Pairs with an odd number of arguments", func() { metadata.Pairs(odd...) })
}

func TestAccessorsLowercaseKeys(t *testing.T) {
	md := metadata.MD{}
	md.Set("Trace-ID", "a")
	md.Append("TRACE-id", "b")
	md.Append("trace-id")
	if got := md.Get("trace-ID"); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("Get = %v, want [a b]", got)
	}
	md.Set("trace-id")
	if got := md.Get("trace-id"); len(got) != 2 {
		t.Fatalf("Set without values changed the key: %v", got)
	}
	md.Set("TRACE-ID", "c")
	if got := md.Get("trace-id"); !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("Set did not replace the values: %v", got)
	}
	md.Delete("Trace-Id")
	if md.Len() != 0 {
		t.Fatalf("Delete left %d keys", md.Len())
	}
}

func TestCopyAndJoin(t *testing.T) {
	md := metadata.Pairs("k", "1")
	cp := md.Copy()
	cp["k"][0] = "changed"
	cp.Append("k", "2")
	if got := md.Get("k"); !reflect.DeepEqual(got, []string{"1"}) {
		t.Fatalf("changing the copy changed the original: %v", got)
	}

	joined := metadata.Join(metadata.Pairs("k", "1"), metadata.Pairs("k", "2", "x", "y"))
	want := metadata.MD{"k": {"1", "2"}, "x": {"y"}}
	if !reflect.DeepEqual(joined, want) {
		t.Fatalf("Join = %v, want %v", map[string][]string(joined), map[string][]string(want))
	}
}

func TestStringHidesContents(t *testing.T) {
	s := metadata.Pairs("authorization", "Bearer secret", "x-user", "42").String()
	if strings.Contains(s, "secret") || strings.Contains(s, "authorization") || s != "map[<2 redacted>]" {
		t.Fatalf("String() = %q, want only the number of keys", s)
	}
}

func TestOutgoingContext(t *testing.T) {
	base := metadata.NewOutgoingContext(context.Background(), metadata.MD{"Authorization": {"Bearer x"}})
	appended := metadata.AppendToOutgoingContext(base, "X-Ids", "1", "x-ids", "2")
	again := metadata.AppendToOutgoingContext(appended, "x-ids", "3")

	got, ok := metadata.FromOutgoingContext(again)
	want := metadata.MD{"authorization": {"Bearer x"}, "x-ids": {"1", "2", "3"}}
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("FromOutgoingContext = %v, %v; want %v", map[string][]string(got), ok, map[string][]string(want))
	}
	if got, _ := metadata.FromOutgoingContext(appended); len(got["x-ids"]) != 2 {
		t.Fatalf("appending to a derived context changed its parent: %v", map[string][]string(got))
	}

	got["authorization"][0] = "changed"
	if again, _ := metadata.FromOutgoingContext(again); again["authorization"][0] != "Bearer x" {
		t.Fatal("changing the returned metadata changed the context")
	}

	onlyAppended := metadata.AppendToOutgoingContext(context.Background(), "k", "v")
	if got, ok := metadata.FromOutgoingContext(onlyAppended); !ok || !reflect.DeepEqual(got, metadata.MD{"k": {"v"}}) {
		t.Fatalf("FromOutgoingContext after only appending = %v, %v", map[string][]string(got), ok)
	}
	if _, ok := metadata.FromOutgoingContext(context.Background()); ok {
		t.Fatal("FromOutgoingContext found metadata in an empty context")
	}
	odd := []string{"k"}
	mustPanic(t, "AppendToOutgoingContext with an odd number of arguments", func() {
		metadata.AppendToOutgoingContext(context.Background(), odd...)
	})
}

func TestIncomingContext(t *testing.T) {
	ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{"X-Ids": {"1", "2"}, "k": {"v"}})

	got, ok := metadata.FromIncomingContext(ctx)
	if !ok || !reflect.DeepEqual(got, metadata.MD{"x-ids": {"1", "2"}, "k": {"v"}}) {
		t.Fatalf("FromIncomingContext = %v, %v", map[string][]string(got), ok)
	}
	got["k"][0] = "changed"
	if v := metadata.ValueFromIncomingContext(ctx, "k"); !reflect.DeepEqual(v, []string{"v"}) {
		t.Fatalf("changing the returned metadata changed the context: %v", v)
	}
	if v := metadata.ValueFromIncomingContext(ctx, "x-IDS"); !reflect.DeepEqual(v, []string{"1", "2"}) {
		t.Fatalf("case-insensitive ValueFromIncomingContext = %v", v)
	}
	if v := metadata.ValueFromIncomingContext(ctx, "missing"); v != nil {
		t.Fatalf("ValueFromIncomingContext of a missing key = %v", v)
	}
	if _, ok := metadata.FromIncomingContext(context.Background()); ok {
		t.Fatal("FromIncomingContext found metadata in an empty context")
	}
	if v := metadata.ValueFromIncomingContext(context.Background(), "k"); v != nil {
		t.Fatalf("ValueFromIncomingContext in an empty context = %v", v)
	}
}

func mustPanic(t *testing.T, what string, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s did not panic", what)
		}
	}()
	fn()
}
