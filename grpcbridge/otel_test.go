package grpcbridge_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/grpcbridge"
)

// TestOtelgrpc traces calls with otelgrpc on both sides, as the services
// of the messenger do: the server span is a child of the client span in the
// same trace, and both carry the attribute that marks the transport.
func TestOtelgrpc(t *testing.T) {
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	opts := []otelgrpc.Option{
		otelgrpc.WithTracerProvider(tp), otelgrpc.WithPropagators(propagation.TraceContext{}),
		otelgrpc.WithSpanAttributes(attribute.String("rpc.transport", "quic")),
	}
	handlerSpans := make(chan trace.SpanContext, 2)
	cc, _ := serve(t, func(ctx context.Context, in *wrapperspb.StringValue) (*wrapperspb.StringValue, error) {
		handlerSpans <- trace.SpanContextFromContext(ctx)
		if in.GetValue() == "missing" {
			return nil, grpcstatus.Error(grpccodes.NotFound, "no such thing")
		}
		return in, nil
	}, []grpcbridge.ServerOption{grpcbridge.StatsHandler(otelgrpc.NewServerHandler(opts...))},
		grpcbridge.WithStatsHandler(otelgrpc.NewClientHandler(opts...)))

	for _, in := range []string{"x", "missing"} {
		_, _ = call(ctx(t), cc, in)
	}
	ended := spans.Ended()
	if len(ended) != 4 {
		t.Fatalf("%d spans, want a client and a server span for each of 2 calls", len(ended))
	}
	byTrace := map[trace.TraceID][]sdktrace.ReadOnlySpan{}
	for _, s := range ended {
		byTrace[s.SpanContext().TraceID()] = append(byTrace[s.SpanContext().TraceID()], s)
	}
	for _, code := range []string{"OK", "NOT_FOUND"} {
		hs := <-handlerSpans
		pair := byTrace[hs.TraceID()]
		if len(pair) != 2 {
			t.Fatalf("trace of the handler has %d spans", len(pair))
		}
		slices.SortFunc(pair, func(a, b sdktrace.ReadOnlySpan) int { return int(a.SpanKind()) - int(b.SpanKind()) })
		server, client := pair[0], pair[1]
		if server.SpanKind() != trace.SpanKindServer || client.SpanKind() != trace.SpanKindClient {
			t.Fatalf("span kinds %v, %v", server.SpanKind(), client.SpanKind())
		}
		if server.Parent().SpanID() != client.SpanContext().SpanID() || server.SpanContext().SpanID() != hs.SpanID() {
			t.Errorf("server span %v with parent %v, client span %v, handler in %v",
				server.SpanContext().SpanID(), server.Parent().SpanID(), client.SpanContext().SpanID(), hs.SpanID())
		}
		for _, s := range pair {
			attrs := map[string]string{}
			for _, kv := range s.Attributes() {
				attrs[string(kv.Key)] = kv.Value.String()
			}
			if s.Name() != strings.TrimPrefix(callMethod, "/") || attrs["rpc.transport"] != "quic" || !hasValue(attrs, "status_code", code) {
				t.Errorf("%v span %q: %v", s.SpanKind(), s.Name(), attrs)
			}
		}
	}
}

// hasValue reports whether an attribute whose key contains part has value.
func hasValue(attrs map[string]string, part, value string) bool {
	for k, v := range attrs {
		if strings.Contains(k, part) && v == value {
			return true
		}
	}
	return false
}
