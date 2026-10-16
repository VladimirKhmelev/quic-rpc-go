package rpc_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

type bidiClient = rpc.BidiStreamingClient[msg, msg]

const bidiMethod = "/test.v1.Echo/Bidi"

func (e *env) bidi(ctx context.Context) (bidiClient, error) {
	cs, err := e.cc.NewStream(ctx, &echoDesc.Streams[0], bidiMethod)
	if err != nil {
		return nil, err
	}
	return &rpc.GenericClientStream[msg, msg]{ClientStream: cs}, nil
}

// mustBidi(t)(e.bidi(ctx)) fails the test if the stream did not open.
func mustBidi(t *testing.T) func(bidiClient, error) bidiClient {
	return func(s bidiClient, err error) bidiClient {
		t.Helper()
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		return s
	}
}

func TestBidiEcho(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{})
		s := mustBidi(t)(e.bidi(transporttest.Context(t)))
		for i := range 10 {
			in := fmt.Sprintf("m%d", i)
			if err := s.Send(wrapperspb.String(in)); err != nil {
				t.Fatalf("Send: %v", err)
			}
			got, err := s.Recv()
			if err != nil || got.GetValue() != in {
				t.Fatalf("Recv: %v, %v; want %q", got, err, in)
			}
		}
		if err := s.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		if _, err := s.Recv(); !errors.Is(err, io.EOF) {
			t.Fatalf("Recv after the server returned: %v, want io.EOF", err)
		}
		if err := s.Send(wrapperspb.String("late")); !errors.Is(err, io.EOF) {
			t.Fatalf("Send after the call ended: %v, want io.EOF", err)
		}
	})
}

// TestBidiHalfClose checks RPC-7: the client closes its side and the
// server keeps sending.
func TestBidiHalfClose(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{bidi: func(stream bidiServer) error {
			var got []string
			for {
				in, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					return err
				}
				got = append(got, in.GetValue())
			}
			for _, v := range got {
				if err := stream.Send(wrapperspb.String(strings.ToUpper(v))); err != nil {
					return err
				}
			}
			return nil
		}})
		s := mustBidi(t)(e.bidi(transporttest.Context(t)))
		for _, v := range []string{"a", "b", "c"} {
			if err := s.Send(wrapperspb.String(v)); err != nil {
				t.Fatalf("Send: %v", err)
			}
		}
		if err := s.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		for _, want := range []string{"A", "B", "C"} {
			if got, err := s.Recv(); err != nil || got.GetValue() != want {
				t.Fatalf("Recv: %v, %v; want %q", got, err, want)
			}
		}
		if _, err := s.Recv(); !errors.Is(err, io.EOF) {
			t.Fatalf("final Recv: %v, want io.EOF", err)
		}
	})
}

func TestBidiServerSpeaksFirst(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{bidi: func(stream bidiServer) error {
			return stream.Send(wrapperspb.String("hello"))
		}})
		s := mustBidi(t)(e.bidi(transporttest.Context(t)))
		if got, err := s.Recv(); err != nil || got.GetValue() != "hello" {
			t.Fatalf("Recv: %v, %v; want hello before the client sent anything", got, err)
		}
		if _, err := s.Recv(); !errors.Is(err, io.EOF) {
			t.Fatalf("final Recv: %v, want io.EOF", err)
		}
	})
}

func TestBidiHeaderWhileReceiving(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{bidi: func(stream bidiServer) error {
			if err := stream.SendHeader(metadata.Pairs("x-session", "s1")); err != nil {
				return err
			}
			stream.SetTrailer(metadata.Pairs("x-count", "1"))
			return echoAll(stream)
		}})
		s := mustBidi(t)(e.bidi(transporttest.Context(t)))

		// Another goroutine is blocked in Recv; Header must still return as
		// soon as the server's HEADERS arrive, before any message.
		received := make(chan error, 1)
		go func() {
			_, err := s.Recv()
			received <- err
		}()
		header, err := s.Header()
		if err != nil || len(header.Get("x-session")) != 1 || header.Get("x-session")[0] != "s1" {
			t.Fatalf("Header: %v, %v; want x-session=s1", map[string][]string(header), err)
		}
		if err := s.Send(wrapperspb.String("x")); err != nil {
			t.Fatalf("Send: %v", err)
		}
		if err := <-received; err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if err := s.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
		if _, err := s.Recv(); !errors.Is(err, io.EOF) {
			t.Fatalf("final Recv: %v, want io.EOF", err)
		}
		if got := s.Trailer().Get("x-count"); len(got) != 1 || got[0] != "1" {
			t.Fatalf("Trailer x-count = %q, want [1]", got)
		}
	})
}

func TestTrailersOnlyResponse(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{bidi: func(bidiServer) error {
			return status.Error(codes.PermissionDenied, "no access")
		}})
		s := mustBidi(t)(e.bidi(transporttest.Context(t)))
		if header, err := s.Header(); header != nil || err != nil {
			t.Fatalf("Header of a trailers-only response: %v, %v; want nil, nil", map[string][]string(header), err)
		}
		_, err := s.Recv()
		wantCode(t, err, codes.PermissionDenied, "Recv")
	})
}

func TestBidiServerFailsMidStream(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{bidi: func(stream bidiServer) error {
			if _, err := stream.Recv(); err != nil {
				return err
			}
			return status.Error(codes.ResourceExhausted, "quota")
		}})
		s := mustBidi(t)(e.bidi(transporttest.Context(t)))
		if err := s.Send(wrapperspb.String("first")); err != nil {
			t.Fatalf("Send: %v", err)
		}
		_, err := s.Recv()
		wantCode(t, err, codes.ResourceExhausted, "Recv")
		if err := s.Send(wrapperspb.String("after the end")); !errors.Is(err, io.EOF) {
			t.Fatalf("Send after the server ended the call: %v, want io.EOF", err)
		}
	})
}
