package rpc_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

// trace records the steps of a call on both sides, in order.
type trace struct {
	mu    sync.Mutex
	steps []string
}

func (tr *trace) add(step string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.steps = append(tr.steps, step)
}

func (tr *trace) want(t *testing.T, want ...string) {
	t.Helper()
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if !slices.Equal(tr.steps, want) {
		t.Errorf("steps %q, want %q", tr.steps, want)
	}
}

func (tr *trace) unaryServer(name string) rpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *rpc.UnaryServerInfo, handler rpc.UnaryHandler) (any, error) {
		tr.add(name + " " + info.FullMethod)
		resp, err := handler(ctx, req)
		tr.add("/" + name)
		return resp, err
	}
}

func (tr *trace) unaryClient(name string) rpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *rpc.ClientConn, invoker rpc.UnaryInvoker, opts ...rpc.CallOption) error {
		tr.add(name + " " + method)
		err := invoker(ctx, method, req, reply, cc, opts...)
		tr.add("/" + name)
		return err
	}
}

func (tr *trace) streamServer(name string) rpc.StreamServerInterceptor {
	return func(srv any, ss rpc.ServerStream, info *rpc.StreamServerInfo, handler rpc.StreamHandler) error {
		tr.add(name + " " + info.FullMethod)
		err := handler(srv, ss)
		tr.add("/" + name)
		return err
	}
}

func (tr *trace) streamClient(name string) rpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *rpc.StreamDesc, cc *rpc.ClientConn, method string, streamer rpc.Streamer, opts ...rpc.CallOption) (rpc.ClientStream, error) {
		tr.add(name + " " + method)
		cs, err := streamer(ctx, desc, cc, method, opts...)
		tr.add("/" + name)
		return cs, err
	}
}

// TestUnaryInterceptors: the chains run on both sides, the first
// interceptor outermost and the options in their order.
func TestUnaryInterceptors(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		var tr trace
		impl := &testServer{unary: func(_ context.Context, in *msg) (*msg, error) {
			tr.add("handler")
			return in, nil
		}}
		e := newEnvWithOptions(t, f, impl,
			[]rpc.ServerOption{rpc.ChainUnaryInterceptor(tr.unaryServer("a"), tr.unaryServer("b")), rpc.ChainUnaryInterceptor(tr.unaryServer("c"))},
			[]rpc.DialOption{rpc.WithChainUnaryInterceptor(tr.unaryClient("x"), tr.unaryClient("y")), rpc.WithChainUnaryInterceptor(tr.unaryClient("z"))})
		if out, err := e.unary(transporttest.Context(t), "hi"); err != nil || out != "hi" {
			t.Fatalf("call: %q, %v", out, err)
		}
		m := " " + unaryMethod
		tr.want(t, "x"+m, "y"+m, "z"+m, "a"+m, "b"+m, "c"+m, "handler", "/c", "/b", "/a", "/z", "/y", "/x")
	})
}

// TestStreamInterceptors: the chains run on both sides around a streaming
// call; on the server they wrap the handler, which ends with the call, on
// the client the start of the call.
func TestStreamInterceptors(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		var tr trace
		impl := &testServer{bidi: func(stream bidiServer) error {
			tr.add("handler")
			return echoAll(stream)
		}}
		e := newEnvWithOptions(t, f, impl,
			[]rpc.ServerOption{rpc.ChainStreamInterceptor(tr.streamServer("a"), tr.streamServer("b")), rpc.ChainStreamInterceptor(tr.streamServer("c"))},
			[]rpc.DialOption{rpc.WithChainStreamInterceptor(tr.streamClient("x"), tr.streamClient("y")), rpc.WithChainStreamInterceptor(tr.streamClient("z"))})
		stream, err := e.cc.NewStream(transporttest.Context(t), &echoDesc.Streams[0], bidiMethod)
		if err != nil {
			t.Fatal(err)
		}
		m := " " + bidiMethod
		tr.want(t, "x"+m, "y"+m, "z"+m, "/z", "/y", "/x")
		if err := stream.CloseSend(); err != nil {
			t.Fatal(err)
		}
		if err := stream.RecvMsg(new(msg)); !errors.Is(err, io.EOF) {
			t.Fatalf("Recv: %v", err)
		}
		tr.want(t, "x"+m, "y"+m, "z"+m, "/z", "/y", "/x", "a"+m, "b"+m, "c"+m, "handler", "/c", "/b", "/a")
	})
}

// TestStreamServerInfo: a streaming interceptor learns which side streams.
func TestStreamServerInfo(t *testing.T) {
	var got []rpc.StreamServerInfo
	srv := rpc.NewServer(rpc.ChainStreamInterceptor(func(srv any, ss rpc.ServerStream, info *rpc.StreamServerInfo, handler rpc.StreamHandler) error {
		got = append(got, *info)
		return handler(srv, ss)
	}))
	desc := rpc.ServiceDesc{ServiceName: "test.v1.Kinds", Streams: []rpc.StreamDesc{
		{StreamName: "Server", ServerStreams: true, Handler: func(any, rpc.ServerStream) error { return nil }},
		{StreamName: "Client", ClientStreams: true, Handler: func(any, rpc.ServerStream) error { return status.Error(codes.Aborted, "") }},
	}}
	srv.RegisterService(&desc, nil)
	l, d := transporttest.Mem(t, 10)
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	cc := rpc.NewClient(d)
	t.Cleanup(func() { _ = cc.Close() })
	for _, sd := range desc.Streams {
		stream, err := cc.NewStream(transporttest.Context(t), &sd, "/test.v1.Kinds/"+sd.StreamName)
		if err != nil {
			t.Fatal(err)
		}
		_ = stream.CloseSend()
		// The handlers send nothing: the first receive ends the call.
		if err := stream.RecvMsg(new(msg)); err == nil {
			t.Fatalf("%s sent a message", sd.StreamName)
		}
	}
	want := []rpc.StreamServerInfo{
		{FullMethod: "/test.v1.Kinds/Server", IsServerStream: true},
		{FullMethod: "/test.v1.Kinds/Client", IsClientStream: true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("interceptor saw %+v, want %+v", got, want)
	}
}

type authKey struct{}

// TestInterceptorsCarryCalls: interceptors see and change what a call
// carries. The client's adds a token to the metadata and changes the reply;
// the server's checks the token, passes the user on in the context, sees the
// client's deadline and changes the response.
func TestInterceptorsCarryCalls(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		auth := func(ctx context.Context, req any, _ *rpc.UnaryServerInfo, handler rpc.UnaryHandler) (any, error) {
			md, _ := metadata.FromIncomingContext(ctx)
			if v := md.Get("authorization"); len(v) != 1 || v[0] != "Bearer alice" {
				return nil, status.Error(codes.Unauthenticated, "no token")
			}
			if _, ok := ctx.Deadline(); !ok {
				return nil, status.Error(codes.InvalidArgument, "no deadline")
			}
			resp, err := handler(context.WithValue(ctx, authKey{}, "alice"), req)
			if err != nil {
				return nil, err
			}
			return wrapperspb.String(resp.(*msg).GetValue() + " (checked)"), nil
		}
		token := func(ctx context.Context, method string, req, reply any, cc *rpc.ClientConn, invoker rpc.UnaryInvoker, opts ...rpc.CallOption) error {
			if err := invoker(metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer alice"), method, req, reply, cc, opts...); err != nil {
				return err
			}
			reply.(*msg).Value += " (received)"
			return nil
		}
		impl := &testServer{unary: func(ctx context.Context, in *msg) (*msg, error) {
			user, _ := ctx.Value(authKey{}).(string)
			return wrapperspb.String(in.GetValue() + " for " + user), nil
		}}
		withToken := newEnvWithOptions(t, f, impl, []rpc.ServerOption{rpc.ChainUnaryInterceptor(auth)}, []rpc.DialOption{rpc.WithChainUnaryInterceptor(token)})
		ctx, cancel := context.WithTimeout(transporttest.Context(t), time.Minute)
		defer cancel()
		if out, err := withToken.unary(ctx, "hi"); err != nil || out != "hi for alice (checked) (received)" {
			t.Fatalf("call with a token: %q, %v", out, err)
		}
		without := newEnvWithOptions(t, f, impl, []rpc.ServerOption{rpc.ChainUnaryInterceptor(auth)}, nil)
		_, err := without.unary(ctx, "hi")
		wantCode(t, err, codes.Unauthenticated, "call without a token")
	})
}

// TestInterceptorsAbort: an interceptor that returns without going on ends
// the call: on the server the handler does not run and the client gets the
// interceptor's status; on the client nothing is sent, not even a dial.
func TestInterceptorsAbort(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		var handled atomic.Int32
		impl := &testServer{
			unary: func(_ context.Context, in *msg) (*msg, error) { handled.Add(1); return in, nil },
			bidi:  func(bidiServer) error { handled.Add(1); return nil },
		}
		denied := status.Error(codes.PermissionDenied, "denied")
		l, d := f(t, 10)
		srv := rpc.NewServer(
			rpc.ChainUnaryInterceptor(func(context.Context, any, *rpc.UnaryServerInfo, rpc.UnaryHandler) (any, error) { return nil, denied }),
			rpc.ChainStreamInterceptor(func(any, rpc.ServerStream, *rpc.StreamServerInfo, rpc.StreamHandler) error { return denied }))
		srv.RegisterService(&echoDesc, impl)
		go func() { _ = srv.Serve(l) }()
		t.Cleanup(srv.Stop)
		ctx := transporttest.Context(t)

		cc := rpc.NewClient(d)
		t.Cleanup(func() { _ = cc.Close() })
		err := cc.Invoke(ctx, unaryMethod, wrapperspb.String("x"), new(msg))
		wantCode(t, err, codes.PermissionDenied, "unary call")
		stream, err := cc.NewStream(ctx, &echoDesc.Streams[0], bidiMethod)
		if err != nil {
			t.Fatal(err)
		}
		wantCode(t, stream.RecvMsg(new(msg)), codes.PermissionDenied, "streaming call")
		if n := handled.Load(); n != 0 {
			t.Errorf("%d handlers ran", n)
		}

		failed := errors.New("not sent")
		dialer := &countingDialer{d: d}
		stopped := rpc.NewClient(dialer,
			rpc.WithChainUnaryInterceptor(func(context.Context, string, any, any, *rpc.ClientConn, rpc.UnaryInvoker, ...rpc.CallOption) error {
				return failed
			}),
			rpc.WithChainStreamInterceptor(func(context.Context, *rpc.StreamDesc, *rpc.ClientConn, string, rpc.Streamer, ...rpc.CallOption) (rpc.ClientStream, error) {
				return nil, failed
			}))
		t.Cleanup(func() { _ = stopped.Close() })
		if err := stopped.Invoke(ctx, unaryMethod, wrapperspb.String("x"), new(msg)); !errors.Is(err, failed) {
			t.Errorf("unary call: %v", err)
		}
		if _, err := stopped.NewStream(ctx, &echoDesc.Streams[0], bidiMethod); !errors.Is(err, failed) {
			t.Errorf("streaming call: %v", err)
		}
		if n := dialer.dials.Load(); n != 0 {
			t.Errorf("dialed %d times", n)
		}
	})
}

// countingServerStream counts what the handler receives and sends, and
// gives it its own context.
type countingServerStream struct {
	rpc.ServerStream
	ctx            context.Context
	received, sent atomic.Int32
}

func (s *countingServerStream) Context() context.Context { return s.ctx }

func (s *countingServerStream) RecvMsg(m any) error {
	err := s.ServerStream.RecvMsg(m)
	if err == nil {
		s.received.Add(1)
	}
	return err
}

func (s *countingServerStream) SendMsg(m any) error {
	s.sent.Add(1)
	return s.ServerStream.SendMsg(m)
}

// countingClientStream counts what the caller sends and receives.
type countingClientStream struct {
	rpc.ClientStream
	sent, received *atomic.Int32
}

func (s *countingClientStream) SendMsg(m any) error {
	s.sent.Add(1)
	return s.ClientStream.SendMsg(m)
}

func (s *countingClientStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err == nil {
		s.received.Add(1)
	}
	return err
}

// TestStreamInterceptorsWrap: interceptors wrap the streams; the handler and
// the caller use the wrappers. Header metadata set through the wrapper's
// context still reaches the client.
func TestStreamInterceptorsWrap(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		var wrapped atomic.Pointer[countingServerStream]
		server := func(srv any, ss rpc.ServerStream, _ *rpc.StreamServerInfo, handler rpc.StreamHandler) error {
			w := &countingServerStream{ServerStream: ss, ctx: context.WithValue(ss.Context(), authKey{}, "alice")}
			wrapped.Store(w)
			return handler(srv, w)
		}
		var sent, received atomic.Int32
		client := func(ctx context.Context, desc *rpc.StreamDesc, cc *rpc.ClientConn, method string, streamer rpc.Streamer, opts ...rpc.CallOption) (rpc.ClientStream, error) {
			cs, err := streamer(ctx, desc, cc, method, opts...)
			if err != nil {
				return nil, err
			}
			return &countingClientStream{ClientStream: cs, sent: &sent, received: &received}, nil
		}
		impl := &testServer{bidi: func(stream bidiServer) error {
			user, _ := stream.Context().Value(authKey{}).(string)
			if err := rpc.SetHeader(stream.Context(), metadata.Pairs("user", user)); err != nil {
				return err
			}
			for {
				in, err := stream.Recv()
				if errors.Is(err, io.EOF) {
					return nil
				}
				if err != nil {
					return err
				}
				if err := stream.Send(wrapperspb.String(in.GetValue() + " for " + user)); err != nil {
					return err
				}
			}
		}}
		e := newEnvWithOptions(t, f, impl, []rpc.ServerOption{rpc.ChainStreamInterceptor(server)}, []rpc.DialOption{rpc.WithChainStreamInterceptor(client)})
		stream, err := e.cc.NewStream(transporttest.Context(t), &echoDesc.Streams[0], bidiMethod)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range []string{"a", "b"} {
			if err := stream.SendMsg(wrapperspb.String(v)); err != nil {
				t.Fatal(err)
			}
			got := new(msg)
			if err := stream.RecvMsg(got); err != nil || got.GetValue() != v+" for alice" {
				t.Fatalf("Recv: %q, %v", got.GetValue(), err)
			}
		}
		if err := stream.CloseSend(); err != nil {
			t.Fatal(err)
		}
		if err := stream.RecvMsg(new(msg)); !errors.Is(err, io.EOF) {
			t.Fatalf("final Recv: %v", err)
		}
		if h, err := stream.Header(); err != nil || h.Get("user")[0] != "alice" {
			t.Errorf("header %v, %v", h, err)
		}
		w := wrapped.Load()
		if w.received.Load() != 2 || w.sent.Load() != 2 || sent.Load() != 2 || received.Load() != 2 {
			t.Errorf("server received %d and sent %d, client sent %d and received %d; want 2 each",
				w.received.Load(), w.sent.Load(), sent.Load(), received.Load())
		}
	})
}

// TestInterceptorsSeeCancellation: the context a server interceptor passes
// on is canceled when the client cancels the call.
func TestInterceptorsSeeCancellation(t *testing.T) {
	forEachTransport(t, func(t *testing.T, f transporttest.Factory) {
		started := make(chan struct{})
		seen := make(chan error, 1)
		interceptor := func(ctx context.Context, req any, _ *rpc.UnaryServerInfo, handler rpc.UnaryHandler) (any, error) {
			resp, err := handler(ctx, req)
			seen <- ctx.Err()
			return resp, err
		}
		impl := &testServer{unary: func(ctx context.Context, _ *msg) (*msg, error) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		e := newEnvWithOptions(t, f, impl, []rpc.ServerOption{rpc.ChainUnaryInterceptor(interceptor)}, nil)
		ctx, cancel := context.WithCancel(transporttest.Context(t))
		result := make(chan error, 1)
		go func() { _, err := e.unary(ctx, "x"); result <- err }()
		<-started
		cancel()
		wantCode(t, <-result, codes.Canceled, "canceled call")
		if err := <-seen; !errors.Is(err, context.Canceled) {
			t.Errorf("the interceptor's context ended with %v", err)
		}
	})
}

// TestNoInterceptorsForUnknownMethods: a call to a method the server does
// not have fails with Unimplemented before any interceptor.
func TestNoInterceptorsForUnknownMethods(t *testing.T) {
	var calls atomic.Int32
	e := newEnvWithOptions(t, transporttest.Mem, &testServer{}, []rpc.ServerOption{
		rpc.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *rpc.UnaryServerInfo, handler rpc.UnaryHandler) (any, error) {
			calls.Add(1)
			return handler(ctx, req)
		}),
		rpc.ChainStreamInterceptor(func(srv any, ss rpc.ServerStream, _ *rpc.StreamServerInfo, handler rpc.StreamHandler) error {
			calls.Add(1)
			return handler(srv, ss)
		}),
	}, nil)
	err := e.cc.Invoke(transporttest.Context(t), "/test.v1.Echo/Missing", wrapperspb.String("x"), new(msg))
	wantCode(t, err, codes.Unimplemented, "call to a missing method")
	if n := calls.Load(); n != 0 {
		t.Errorf("interceptors ran %d times", n)
	}
}
