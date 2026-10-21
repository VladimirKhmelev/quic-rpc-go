package grpcbridge_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	grpcmetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/grpcbridge"
)

// eventLog collects what the handlers and the code around them see.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(format string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, fmt.Sprintf(format, a...))
}

func (l *eventLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	events := l.events
	l.events = nil
	return events
}

type tagKey struct{ name string }

// recorder is a stats handler that logs its events. On the client it adds
// outgoing metadata in TagRPC, as otelgrpc adds its trace context.
type recorder struct {
	name string
	log  *eventLog
}

func (r *recorder) TagRPC(ctx context.Context, info *stats.RPCTagInfo) context.Context {
	r.log.add("%s tag %s", r.name, info.FullMethodName)
	if strings.HasPrefix(r.name, "client") {
		ctx = grpcmetadata.AppendToOutgoingContext(ctx, "x-"+r.name, "tagged")
	}
	return context.WithValue(ctx, tagKey{r.name}, true)
}

func (r *recorder) HandleRPC(ctx context.Context, s stats.RPCStats) {
	if ctx.Value(tagKey{r.name}) == nil {
		r.log.add("%s: an event without the context of TagRPC", r.name)
	}
	switch s := s.(type) {
	case *stats.Begin:
		r.log.add("%s begin client=%v streams=%v,%v", r.name, s.Client, s.IsClientStream, s.IsServerStream)
		if s.BeginTime.IsZero() {
			r.log.add("%s: no begin time", r.name)
		}
	case *stats.OutHeader:
		r.log.add("%s out-header %s %v", r.name, s.FullMethod, s.Header.Get("x-client1"))
	case *stats.InHeader:
		r.log.add("%s in-header %s %v", r.name, s.FullMethod, s.Header.Get("x-client1"))
	case *stats.End:
		r.log.add("%s end %v %v", r.name, grpcstatus.Code(s.Error), s.Trailer.Get("x-trailer"))
		if s.EndTime.Before(s.BeginTime) || s.BeginTime.IsZero() {
			r.log.add("%s: end at %v, begin at %v", r.name, s.EndTime, s.BeginTime)
		}
	default:
		r.log.add("%s %T", r.name, s)
	}
}

func (r *recorder) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	r.log.add("%s tag-conn", r.name)
	return ctx
}

func (r *recorder) HandleConn(context.Context, stats.ConnStats) { r.log.add("%s handle-conn", r.name) }

// TestStatsHandlers: the handlers of both sides get their events in grpc-go's
// order, around the interceptors, and what a client handler adds to the
// outgoing metadata reaches the server.
func TestStatsHandlers(t *testing.T) {
	var log eventLog
	cc, _ := serve(t, func(ctx context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
		md, _ := grpcmetadata.FromIncomingContext(ctx)
		log.add("handler %v %v", md.Get("x-client1"), ctx.Value(tagKey{"server2"}) != nil)
		if err := grpc.SetTrailer(ctx, grpcmetadata.Pairs("x-trailer", "t")); err != nil {
			return nil, err
		}
		if in.GetValue() == "missing" {
			return nil, grpcstatus.Error(grpccodes.NotFound, "no such thing")
		}
		return in, nil
	}, []grpcbridge.ServerOption{
		grpcbridge.StatsHandler(&recorder{"server1", &log}), grpcbridge.StatsHandler(nil), grpcbridge.StatsHandler(&recorder{"server2", &log}),
		grpcbridge.ChainUnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			log.add("server interceptor")
			return handler(ctx, req)
		}),
	},
		grpcbridge.WithStatsHandler(&recorder{"client1", &log}), grpcbridge.WithStatsHandler(&recorder{"client2", &log}),
		grpcbridge.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			log.add("client interceptor")
			return invoker(ctx, method, req, reply, cc, opts...)
		}))

	for _, tt := range []struct {
		in   string
		code grpccodes.Code
	}{{"x", grpccodes.OK}, {"missing", grpccodes.NotFound}} {
		_, err := call(ctx(t), cc, tt.in)
		if grpcstatus.Code(err) != tt.code {
			t.Fatalf("call %q: %v", tt.in, err)
		}
		end := fmt.Sprintf(" end %v [t]", tt.code)
		want := []string{
			"client interceptor",
			"client1 tag " + callMethod, "client2 tag " + callMethod,
			"client1 begin client=true streams=false,false", "client2 begin client=true streams=false,false",
			"client1 out-header " + callMethod + " [tagged]", "client2 out-header " + callMethod + " [tagged]",
			"server1 tag " + callMethod, "server2 tag " + callMethod,
			"server1 begin client=false streams=false,false", "server2 begin client=false streams=false,false",
			"server1 in-header " + callMethod + " [tagged]", "server2 in-header " + callMethod + " [tagged]",
			"server interceptor", "handler [tagged] true",
			// The server has the trailer only once the handler returns.
			"server1 end " + tt.code.String() + " []", "server2 end " + tt.code.String() + " []",
			"client1" + end, "client2" + end,
		}
		if got := log.take(); !slices.Equal(got, want) {
			t.Errorf("call %q:\n got %q\nwant %q", tt.in, got, want)
		}
	}
}

// TestStreamStats: a stream begins as a stream and ends once, when RecvMsg
// returns io.EOF or an error, or when its context is done even if nobody
// receives any more.
func TestStreamStats(t *testing.T) {
	var log eventLog
	cc, _ := serve(t, func(_ context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) { return in, nil },
		[]grpcbridge.ServerOption{grpcbridge.StatsHandler(&recorder{"server1", &log})},
		grpcbridge.WithStatsHandler(&recorder{"client1", &log}))

	r, err := repeat(ctx(t), cc, "x")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err = r.Recv(); err != nil {
			break
		}
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Repeat ended with %v", err)
	}
	waitFor(t, &log, "server1 end OK []")
	want := []string{
		"client1 tag " + repeatMethod, "client1 begin client=true streams=false,true", "client1 out-header " + repeatMethod + " [tagged]",
		"server1 tag " + repeatMethod, "server1 begin client=false streams=false,true", "server1 in-header " + repeatMethod + " [tagged]",
		"client1 end OK []", "server1 end OK []",
	}
	if got := log.take(); !sameEvents(got, want) {
		t.Errorf("Repeat:\n got %q\nwant %q in any order", got, want)
	}

	for _, receive := range []bool{true, false} {
		watchCtx, cancel := context.WithCancel(ctx(t))
		w, err := healthpb.NewHealthClient(cc).Watch(watchCtx, &healthpb.HealthCheckRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Recv(); err != nil {
			t.Fatal(err)
		}
		cancel()
		if receive {
			if _, err := w.Recv(); grpcstatus.Code(err) != grpccodes.Canceled {
				t.Fatalf("Recv after cancel: %v", err)
			}
		}
		waitFor(t, &log, "client1 end Canceled []")
		waitFor(t, &log, "server1 end Canceled []")
		got := log.take()
		if n := countPrefix(got, "client1 end"); n != 1 || !slices.Contains(got, "client1 begin client=true streams=false,true") {
			t.Errorf("Watch, receiving after cancel %v: %q", receive, got)
		}
	}
}

// waitFor waits until event is logged.
func waitFor(t *testing.T, l *eventLog, event string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !slices.Contains(logged(l), event); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q in %q", event, logged(l))
		}
	}
}

func sameEvents(got, want []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want)))
}

func logged(l *eventLog) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

func countPrefix(events []string, prefix string) int {
	n := 0
	for _, e := range events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}
