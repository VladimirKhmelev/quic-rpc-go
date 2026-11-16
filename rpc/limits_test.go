package rpc_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
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

// Limits count serialized protobuf bytes, including the field tag and length.
func TestConfiguredMessageLimits(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		for _, direction := range []string{"server-recv", "server-send", "client-recv", "client-send"} {
			t.Run(direction, func(t *testing.T) {
				for _, value := range []string{"", "abc"} {
					t.Run(fmt.Sprintf("size-%d", len(value)), func(t *testing.T) {
						limit := proto.Size(wrapperspb.String(value))
						var so []rpc.ServerOption
						var co []rpc.CallOption
						switch direction {
						case "server-recv":
							so = []rpc.ServerOption{rpc.MaxRecvMsgSize(limit)}
						case "server-send":
							so = []rpc.ServerOption{rpc.MaxSendMsgSize(limit)}
						case "client-recv":
							co = []rpc.CallOption{rpc.MaxCallRecvMsgSize(limit)}
						case "client-send":
							co = []rpc.CallOption{rpc.MaxCallSendMsgSize(limit)}
						}
						e := newEnvWithOptions(t, f, &testServer{}, so, []rpc.DialOption{rpc.WithDefaultCallOptions(co...)})
						for _, stream := range []bool{false, true} {
							for _, in := range []string{value, value + "x", value} {
								var err error
								if !stream {
									_, err = e.unary(transporttest.Context(t), in)
								} else {
									cs, openErr := e.cc.NewStream(transporttest.Context(t), &echoDesc.Streams[0], bidiMethod)
									if openErr != nil {
										t.Fatal(openErr)
									}
									// Send concurrently so the in-memory transport's bounded pipe
									// cannot hide a server rejection behind a blocked write.
									sent := make(chan error, 1)
									go func() { sent <- cs.SendMsg(wrapperspb.String(in)); _ = cs.CloseSend() }()
									err = cs.RecvMsg(new(msg))
									sendErr := <-sent
									if direction == "client-send" && in != value {
										wantCode(t, sendErr, codes.ResourceExhausted, "SendMsg limit")
									}
									if err == nil {
										if end := cs.RecvMsg(new(msg)); !errors.Is(end, io.EOF) {
											t.Fatalf("stream end: %v", end)
										}
									}
								}
								want := codes.OK
								if in != value {
									want = codes.ResourceExhausted
								}
								wantCode(t, err, want, fmt.Sprintf("stream=%v input=%q", stream, in))
							}
						}
					})
				}
			})
		}
	})
}

func TestMessageLimitDefaultsAndOverrides(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		defaults := []rpc.CallOption{rpc.MaxCallSendMsgSize(0), rpc.MaxCallRecvMsgSize(0)}
		option := rpc.WithDefaultCallOptions(defaults...)
		// The option owns its slice; later changes must not alter the client.
		defaults[0] = rpc.MaxCallSendMsgSize(100)
		e := newEnvWithOptions(t, f, &testServer{}, nil, []rpc.DialOption{option})
		_, err := e.unary(transporttest.Context(t), "x")
		wantCode(t, err, codes.ResourceExhausted, "client defaults")
		out, err := e.unary(transporttest.Context(t), "x", rpc.MaxCallSendMsgSize(1), rpc.MaxCallSendMsgSize(3), rpc.MaxCallRecvMsgSize(3))
		if err != nil || out != "x" {
			t.Fatalf("per-call overrides: %q, %v", out, err)
		}
		cs, err := e.cc.NewStream(transporttest.Context(t), &echoDesc.Streams[0], bidiMethod, rpc.MaxCallSendMsgSize(3), rpc.MaxCallRecvMsgSize(3))
		if err != nil {
			t.Fatal(err)
		}
		if err := cs.SendMsg(wrapperspb.String("x")); err != nil {
			t.Fatal(err)
		}
		_ = cs.CloseSend()
		if err := cs.RecvMsg(new(msg)); err != nil {
			t.Fatal(err)
		}
		if err := cs.RecvMsg(new(msg)); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	})
}

func TestInvalidMessageLimits(t *testing.T) {
	cc := rpc.NewClient(nil)
	for _, opt := range []rpc.CallOption{rpc.MaxCallSendMsgSize(-1), rpc.MaxCallRecvMsgSize(-1)} {
		// Invalid options fail before attempting to use the nil dialer.
		err := cc.Invoke(t.Context(), unaryMethod, wrapperspb.String(""), new(msg), opt)
		wantCode(t, err, codes.InvalidArgument, "negative unary limit")
		_, err = cc.NewStream(t.Context(), &echoDesc.Streams[0], bidiMethod, opt)
		wantCode(t, err, codes.InvalidArgument, "negative streaming limit")
	}
	for _, opt := range []func(int) rpc.ServerOption{rpc.MaxRecvMsgSize, rpc.MaxSendMsgSize} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("negative server limit did not panic")
				}
			}()
			opt(-1)
		}()
	}
}

func TestRaisedMessageLimit(t *testing.T) {
	big := strings.Repeat("x", 8<<20)
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnvWithOptions(t, f, &testServer{}, []rpc.ServerOption{rpc.MaxRecvMsgSize(9 << 20), rpc.MaxSendMsgSize(9 << 20)}, []rpc.DialOption{rpc.WithDefaultCallOptions(rpc.MaxCallRecvMsgSize(9<<20), rpc.MaxCallSendMsgSize(9<<20))})
		out, err := e.unary(transporttest.Context(t), big)
		if err != nil || out != big {
			t.Fatalf("8 MiB round trip: len=%d, %v", len(out), err)
		}
	})
}

func TestMessageLimitInterceptorOverride(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		e := newEnvWithOptions(t, f, &testServer{}, nil, []rpc.DialOption{
			rpc.WithDefaultCallOptions(rpc.MaxCallSendMsgSize(0), rpc.MaxCallRecvMsgSize(0)),
			rpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *rpc.ClientConn, invoke rpc.UnaryInvoker, opts ...rpc.CallOption) error {
				return invoke(ctx, method, req, reply, cc, append(opts, rpc.MaxCallSendMsgSize(3), rpc.MaxCallRecvMsgSize(3))...)
			}),
			rpc.WithChainStreamInterceptor(func(ctx context.Context, desc *rpc.StreamDesc, cc *rpc.ClientConn, method string, streamer rpc.Streamer, opts ...rpc.CallOption) (rpc.ClientStream, error) {
				return streamer(ctx, desc, cc, method, append(opts, rpc.MaxCallSendMsgSize(3), rpc.MaxCallRecvMsgSize(3))...)
			}),
		})
		if out, err := e.unary(transporttest.Context(t), "x"); err != nil || out != "x" {
			t.Fatalf("unary interceptor: %q, %v", out, err)
		}
		cs, err := e.cc.NewStream(transporttest.Context(t), &echoDesc.Streams[0], bidiMethod)
		if err != nil {
			t.Fatal(err)
		}
		if err := cs.SendMsg(wrapperspb.String("x")); err != nil {
			t.Fatal(err)
		}
		_ = cs.CloseSend()
		if err := cs.RecvMsg(new(msg)); err != nil {
			t.Fatal(err)
		}
		if err := cs.RecvMsg(new(msg)); !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	})
}
