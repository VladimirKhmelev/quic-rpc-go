package rpc_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
)

func TestCardinalityViolations(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{bidi: func(stream bidiServer) error {
			// Answers with as many messages as the first request says.
			in, err := stream.Recv()
			if err != nil {
				return err
			}
			for range len(in.GetValue()) {
				if err := stream.Send(in); err != nil {
					return err
				}
			}
			return nil
		}})
		ctx := transporttest.Context(t)
		call := func(desc *rpc.StreamDesc, method string, send ...string) error {
			cs, err := e.cc.NewStream(ctx, desc, method)
			if err != nil {
				return err
			}
			for _, v := range send {
				if err := cs.SendMsg(wrapperspb.String(v)); err != nil {
					return err
				}
			}
			if err := cs.CloseSend(); err != nil {
				return err
			}
			return cs.RecvMsg(new(msg))
		}
		clientStreaming := &rpc.StreamDesc{ClientStreams: true}

		err := call(clientStreaming, unaryMethod, "a", "b")
		wantCode(t, err, codes.Internal, "two requests to a unary method")
		err = call(clientStreaming, unaryMethod)
		wantCode(t, err, codes.Internal, "no request to a unary method")
		err = call(clientStreaming, bidiMethod, "xx")
		wantCode(t, err, codes.Internal, "two responses to a non-server-streaming call")
		err = call(clientStreaming, bidiMethod, "")
		wantCode(t, err, codes.Internal, "no response to a non-server-streaming call")
		if err := call(clientStreaming, bidiMethod, "x"); err != nil {
			t.Fatalf("exactly one response: %v", err)
		}
	})
}

func TestMessageTooLarge(t *testing.T) {
	big := strings.Repeat("x", 5<<20)
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnv(t, f, &testServer{unary: func(_ context.Context, in *msg) (*msg, error) {
			if in.GetValue() == "send big" {
				return wrapperspb.String(big), nil
			}
			return in, nil
		}})
		ctx := transporttest.Context(t)
		_, err := e.unary(ctx, big)
		wantCode(t, err, codes.ResourceExhausted, "request over the limit")
		_, err = e.unary(ctx, "send big")
		wantCode(t, err, codes.ResourceExhausted, "response over the limit")
		if _, err := e.unary(ctx, "small"); err != nil {
			t.Fatalf("call after the oversized ones: %v", err)
		}
	})
}

// TestStreamSlotsReleased makes sure finished calls give back their stream
// slot: with a limit of 2 concurrent streams, many sequential calls would
// otherwise block.
func TestStreamSlotsReleased(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnvWithLimit(t, f, &testServer{bidi: func(stream bidiServer) error {
			// Returns without reading the client's FIN.
			_, err := stream.Recv()
			return err
		}}, 2)
		ctx := transporttest.Context(t)
		for i := range 30 {
			if _, err := e.unary(ctx, "x"); err != nil {
				t.Fatalf("unary call %d: %v", i, err)
			}
			// The client never calls CloseSend.
			s := mustBidi(t)(e.bidi(ctx))
			if err := s.Send(wrapperspb.String("x")); err != nil {
				t.Fatalf("bidi call %d Send: %v", i, err)
			}
			if _, err := s.Recv(); !errors.Is(err, io.EOF) {
				t.Fatalf("bidi call %d Recv: %v, want io.EOF", i, err)
			}
		}
	})
}
