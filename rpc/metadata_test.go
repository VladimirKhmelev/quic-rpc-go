package rpc_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

func TestMetadata(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{unary: func(ctx context.Context, in *msg) (*msg, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			if err := rpc.SetHeader(ctx, metadata.Pairs(
				"x-seen", strings.Join(md.Get("x-ids"), ","),
				"trace-echo-bin", strings.Join(md.Get("trace-bin"), ""),
			)); err != nil {
				return nil, err
			}
			if err := rpc.SetTrailer(ctx, metadata.Pairs("x-trailer", "done")); err != nil {
				return nil, err
			}
			return in, nil
		}})

		ctx := metadata.AppendToOutgoingContext(transporttest.Context(t),
			"x-ids", "1", "x-ids", "2", "trace-bin", "\x00\x01\xff")
		var header, trailer metadata.MD
		if _, err := e.unary(ctx, "x", rpc.Header(&header), rpc.Trailer(&trailer)); err != nil {
			t.Fatalf("call: %v", err)
		}
		if got := header.Get("x-seen"); len(got) != 1 || got[0] != "1,2" {
			t.Errorf("header x-seen = %q, want [1,2]", got)
		}
		if got := header.Get("trace-echo-bin"); len(got) != 1 || got[0] != "\x00\x01\xff" {
			t.Errorf("binary metadata did not survive the round trip: %q", got)
		}
		if got := trailer.Get("x-trailer"); len(got) != 1 || got[0] != "done" {
			t.Errorf("trailer x-trailer = %q, want [done]", got)
		}
	})
}

func TestInvalidOutgoingMetadata(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		var called atomic.Bool
		e := newEnv(t, f, &testServer{unary: func(_ context.Context, in *msg) (*msg, error) {
			called.Store(true)
			return in, nil
		}})
		ctx := metadata.NewOutgoingContext(transporttest.Context(t), metadata.MD{"bad key": {"v"}})
		_, err := e.unary(ctx, "x")
		wantCode(t, err, codes.Internal, "call with an invalid metadata key")
		if called.Load() {
			t.Fatal("the handler ran for a call with invalid metadata")
		}
	})
}

func TestServerMetadataFunctionsOutsideACall(t *testing.T) {
	ctx := context.Background()
	md := metadata.Pairs("k", "v")
	for name, err := range map[string]error{
		"SetHeader":  rpc.SetHeader(ctx, md),
		"SendHeader": rpc.SendHeader(ctx, md),
		"SetTrailer": rpc.SetTrailer(ctx, md),
	} {
		if status.Code(err) != codes.Internal {
			t.Errorf("%s outside a call: %v, want Internal", name, err)
		}
	}
}

func TestHeaderCanBeSentOnce(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{bidi: func(stream bidiServer) error {
			if err := stream.SetHeader(metadata.Pairs("a", "1")); err != nil {
				return err
			}
			if err := stream.SendHeader(metadata.Pairs("b", "2")); err != nil {
				return err
			}
			if err := stream.SetHeader(metadata.Pairs("c", "3")); status.Code(err) != codes.Internal {
				return status.Errorf(codes.Unknown, "SetHeader after SendHeader: %v", err)
			}
			if err := stream.SendHeader(nil); status.Code(err) != codes.Internal {
				return status.Errorf(codes.Unknown, "second SendHeader: %v", err)
			}
			return nil
		}})
		s := mustBidi(t)(e.bidi(transporttest.Context(t)))
		header, err := s.Header()
		if err != nil {
			t.Fatalf("Header: %v", err)
		}
		if header.Get("a")[0] != "1" || header.Get("b")[0] != "2" || header.Get("c") != nil {
			t.Fatalf("header %v, want a=1 and b=2 only", map[string][]string(header))
		}
		// The handler reports a failed check as an error status.
		if _, err := s.Recv(); !errors.Is(err, io.EOF) {
			t.Fatalf("Recv: %v, want io.EOF", err)
		}
	})
}
