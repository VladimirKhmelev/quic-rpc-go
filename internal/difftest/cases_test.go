package difftest_test

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	difftestv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/difftest/v1"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
)

type testCase struct {
	name string
	call func(ctx context.Context, c client) outcome
	// timeout and cancelAfter, if set, end the call from the client side.
	timeout     time.Duration
	cancelAfter time.Duration
	// The reference must produce this code and this many responses, so
	// that a broken scenario cannot pass by failing the same way on all
	// stacks.
	wantCode      codes.Code
	wantResponses int
	// codeOnly marks status messages worded by the library itself rather
	// than by the handler; they differ between grpc-go and quic-rpc-go. Each
	// such case says why.
	codeOnly bool
}

func unaryCall(sc *difftestv1.Scenario, md map[string][]string, payload []byte) func(context.Context, client) outcome {
	return func(ctx context.Context, c client) outcome {
		return c.unary(ctx, md, &difftestv1.Request{Scenario: sc, Seq: 1, Payload: payload})
	}
}

func invokeCall(method string) func(context.Context, client) outcome {
	return func(ctx context.Context, c client) outcome { return c.invoke(ctx, method) }
}

// bidiCall sends n requests; the first one carries the scenario.
func bidiCall(sc *difftestv1.Scenario, mode bidiMode, n int) func(context.Context, client) outcome {
	return func(ctx context.Context, c client) outcome {
		reqs := make([]*difftestv1.Request, n)
		for i := range reqs {
			reqs[i] = &difftestv1.Request{Seq: int32(i), Payload: fmt.Appendf(nil, "p%d", i)}
		}
		reqs[0].Scenario = sc
		return c.bidi(ctx, mode, reqs)
	}
}

func serverStreamCall(sc *difftestv1.Scenario, md map[string][]string) func(context.Context, client) outcome {
	return func(ctx context.Context, c client) outcome {
		return c.serverStream(ctx, md, &difftestv1.Request{Scenario: sc, Payload: []byte("ping")})
	}
}

// clientStreamCall sends n requests; the first one carries the scenario.
func clientStreamCall(sc *difftestv1.Scenario, n int) func(context.Context, client) outcome {
	return func(ctx context.Context, c client) outcome { return c.clientStream(ctx, requests(sc, n)) }
}

// rawCall calls method of DiffTest as a bidirectional stream with n
// requests.
func rawCall(method string, sc *difftestv1.Scenario, n int) func(context.Context, client) outcome {
	return func(ctx context.Context, c client) outcome {
		return c.raw(ctx, "/difftest.v1.DiffTest/"+method, requests(sc, n))
	}
}

func requests(sc *difftestv1.Scenario, n int) []*difftestv1.Request {
	reqs := make([]*difftestv1.Request, n)
	for i := range reqs {
		reqs[i] = &difftestv1.Request{Seq: int32(i)}
	}
	if n > 0 {
		reqs[0].Scenario = sc
	}
	return reqs
}

func md(kv ...string) []*difftestv1.Metadata {
	var out []*difftestv1.Metadata
	for i := 0; i < len(kv); i += 2 {
		out = append(out, &difftestv1.Metadata{Key: kv[i], Value: []byte(kv[i+1])})
	}
	return out
}

func cases() []testCase {
	var cs []testCase
	add := func(c testCase) { cs = append(cs, c) }

	add(testCase{name: "unary/ok", call: unaryCall(&difftestv1.Scenario{}, nil, []byte("ping")), wantResponses: 1})
	for code := codes.Canceled; code <= codes.Unauthenticated; code++ {
		add(testCase{
			name:     "unary/code/" + code.String(),
			call:     unaryCall(&difftestv1.Scenario{Code: uint32(code), Message: "failed with " + code.String()}, nil, nil),
			wantCode: code,
		})
	}
	add(testCase{
		name: "unary/details",
		call: unaryCall(&difftestv1.Scenario{
			Code:    uint32(codes.FailedPrecondition),
			Message: "archived",
			Details: []*difftestv1.Detail{{Text: "unarchive first"}, {Text: "or ask an admin"}},
		}, nil, nil),
		wantCode: codes.FailedPrecondition,
	})
	add(testCase{
		name: "unary/metadata",
		call: unaryCall(&difftestv1.Scenario{
			Header:  md("x-h", "a", "x-h", "b", "x-h-bin", "\x00\xff"),
			Trailer: md("x-t", "t1", "x-t-bin", "\x01\x02"),
		}, nil, nil),
		wantResponses: 1,
	})
	add(testCase{
		name: "unary/header-first-then-error",
		call: unaryCall(&difftestv1.Scenario{
			SendHeaderFirst: true,
			Header:          md("x-session", "s1"),
			Code:            uint32(codes.NotFound),
			Message:         "no chat",
		}, nil, nil),
		wantCode: codes.NotFound,
	})
	add(testCase{
		name: "unary/echo-metadata",
		call: unaryCall(&difftestv1.Scenario{EchoMetadata: true},
			map[string][]string{"x-request-id": {"r1", "r2"}, "x-trace-bin": {"\x00\x01\xfe"}}, nil),
		wantResponses: 1,
	})
	add(testCase{
		name:          "unary/large-response",
		call:          unaryCall(&difftestv1.Scenario{PayloadSize: 1 << 20}, nil, nil),
		wantResponses: 1,
	})
	add(testCase{
		name:     "unary/request-too-large",
		call:     unaryCall(&difftestv1.Scenario{}, nil, make([]byte, 5<<20)),
		wantCode: codes.ResourceExhausted,
		codeOnly: true,
	})
	add(testCase{
		name:     "unary/response-too-large",
		call:     unaryCall(&difftestv1.Scenario{PayloadSize: 5 << 20}, nil, nil),
		wantCode: codes.ResourceExhausted,
		codeOnly: true,
	})
	// In the deadline cases the client's timer and the server's reset race.
	// When the reset wins, grpc-go reports "stream terminated by RST_STREAM
	// with error code: CANCEL" (internal/transport/http2_client.go), while
	// quic-rpc-go always reports "context deadline exceeded"; the code is the
	// same either way.
	add(testCase{
		name:     "unary/deadline",
		call:     unaryCall(&difftestv1.Scenario{WaitForCancel: true}, nil, nil),
		timeout:  100 * time.Millisecond,
		wantCode: codes.DeadlineExceeded,
		codeOnly: true,
	})
	add(testCase{
		name:        "unary/cancel",
		call:        unaryCall(&difftestv1.Scenario{WaitForCancel: true}, nil, nil),
		cancelAfter: 100 * time.Millisecond,
		wantCode:    codes.Canceled,
	})
	add(testCase{name: "unary/unknown-method", call: invokeCall("/difftest.v1.DiffTest/Missing"), wantCode: codes.Unimplemented})
	add(testCase{name: "unary/unknown-service", call: invokeCall("/missing.v1.Service/Get"), wantCode: codes.Unimplemented})
	add(testCase{
		name:     "unary/invalid-metadata",
		call:     unaryCall(&difftestv1.Scenario{}, map[string][]string{"bad key": {"v"}}, nil),
		wantCode: codes.Internal,
		codeOnly: true,
	})

	add(testCase{name: "bidi/echo", call: bidiCall(&difftestv1.Scenario{Echo: true}, interleave, 5), wantResponses: 5})
	add(testCase{
		name:          "bidi/half-close",
		call:          bidiCall(&difftestv1.Scenario{ReadAllFirst: true, Responses: 3}, sendAll, 3),
		wantResponses: 3,
	})
	add(testCase{
		name:          "bidi/error-after-responses",
		call:          bidiCall(&difftestv1.Scenario{Responses: 2, Code: uint32(codes.Aborted), Message: "stop"}, sendAll, 1),
		wantCode:      codes.Aborted,
		wantResponses: 2,
	})
	add(testCase{
		name:     "bidi/trailers-only",
		call:     bidiCall(&difftestv1.Scenario{Code: uint32(codes.PermissionDenied), Message: "no access"}, sendAll, 1),
		wantCode: codes.PermissionDenied,
	})
	add(testCase{
		name: "bidi/header-first-and-trailer",
		call: bidiCall(&difftestv1.Scenario{
			SendHeaderFirst: true,
			Header:          md("x-session", "s1"),
			Trailer:         md("x-count", "1"),
			Responses:       1,
			PayloadSize:     16,
		}, sendAll, 1),
		wantResponses: 1,
	})
	add(testCase{
		name:          "bidi/server-ends-first",
		call:          bidiCall(&difftestv1.Scenario{Responses: 1}, sendAll, 5),
		wantResponses: 1,
	})
	add(testCase{
		name:     "bidi/deadline",
		call:     bidiCall(&difftestv1.Scenario{WaitForCancel: true}, sendFirst, 1),
		timeout:  100 * time.Millisecond,
		wantCode: codes.DeadlineExceeded,
		codeOnly: true, // see unary/deadline
	})
	add(testCase{
		name:        "bidi/cancel",
		call:        bidiCall(&difftestv1.Scenario{WaitForCancel: true}, sendFirst, 1),
		cancelAfter: 100 * time.Millisecond,
		wantCode:    codes.Canceled,
	})

	add(testCase{name: "server-stream/ok", call: serverStreamCall(&difftestv1.Scenario{Responses: 3}, nil), wantResponses: 3})
	add(testCase{name: "server-stream/none", call: serverStreamCall(&difftestv1.Scenario{}, nil)})
	add(testCase{
		name:          "server-stream/error-after-responses",
		call:          serverStreamCall(&difftestv1.Scenario{Responses: 2, Code: uint32(codes.Aborted), Message: "stop"}, nil),
		wantCode:      codes.Aborted,
		wantResponses: 2,
	})
	add(testCase{
		name: "server-stream/trailers-only",
		call: serverStreamCall(&difftestv1.Scenario{
			Code:    uint32(codes.PermissionDenied),
			Message: "no access",
			Details: []*difftestv1.Detail{{Text: "ask an admin"}},
		}, nil),
		wantCode: codes.PermissionDenied,
	})
	add(testCase{
		name: "server-stream/metadata",
		call: serverStreamCall(&difftestv1.Scenario{
			SendHeaderFirst: true,
			EchoMetadata:    true,
			Header:          md("x-session", "s1"),
			Trailer:         md("x-count", "2", "x-t-bin", "\x01\x02"),
			Responses:       2,
			PayloadSize:     16,
		}, map[string][]string{"x-request-id": {"r1"}}),
		wantResponses: 2,
	})
	add(testCase{
		name:     "server-stream/deadline",
		call:     serverStreamCall(&difftestv1.Scenario{WaitForCancel: true}, nil),
		timeout:  100 * time.Millisecond,
		wantCode: codes.DeadlineExceeded,
		codeOnly: true, // see unary/deadline
	})
	add(testCase{
		name:        "server-stream/cancel",
		call:        serverStreamCall(&difftestv1.Scenario{WaitForCancel: true}, nil),
		cancelAfter: 100 * time.Millisecond,
		wantCode:    codes.Canceled,
	})
	// A server-streaming method takes exactly one request; the server's
	// RecvMsg reports the broken rule, with the same words in both
	// libraries.
	add(testCase{name: "server-stream/two-requests", call: rawCall("ServerStream", &difftestv1.Scenario{Responses: 1}, 2), wantCode: codes.Internal})
	add(testCase{name: "server-stream/no-request", call: rawCall("ServerStream", nil, 0), wantCode: codes.Internal})

	add(testCase{
		name:          "client-stream/ok",
		call:          clientStreamCall(&difftestv1.Scenario{ReadAllFirst: true, Responses: 1}, 3),
		wantResponses: 1,
	})
	add(testCase{name: "client-stream/no-requests", call: clientStreamCall(nil, 0), wantResponses: 1})
	add(testCase{
		name:          "client-stream/server-ends-first",
		call:          clientStreamCall(&difftestv1.Scenario{Responses: 1, PayloadSize: 16}, 5),
		wantResponses: 1,
	})
	add(testCase{
		name:     "client-stream/error",
		call:     clientStreamCall(&difftestv1.Scenario{ReadAllFirst: true, Code: uint32(codes.InvalidArgument), Message: "bad batch"}, 3),
		wantCode: codes.InvalidArgument,
	})
	add(testCase{
		name: "client-stream/metadata",
		call: clientStreamCall(&difftestv1.Scenario{
			ReadAllFirst:    true,
			SendHeaderFirst: true,
			Header:          md("x-session", "s1"),
			Trailer:         md("x-count", "2"),
			Responses:       1,
		}, 2),
		wantResponses: 1,
	})
	add(testCase{
		name:     "client-stream/deadline",
		call:     clientStreamCall(&difftestv1.Scenario{WaitForCancel: true}, 1),
		timeout:  100 * time.Millisecond,
		wantCode: codes.DeadlineExceeded,
		codeOnly: true, // see unary/deadline
	})
	add(testCase{
		name:        "client-stream/cancel",
		call:        clientStreamCall(&difftestv1.Scenario{WaitForCancel: true}, 1),
		cancelAfter: 100 * time.Millisecond,
		wantCode:    codes.Canceled,
	})
	// A client-streaming method answers exactly once; the client reports a
	// missing or an extra answer, with the same words in both libraries.
	add(testCase{name: "client-stream/no-response", call: clientStreamCall(&difftestv1.Scenario{ReadAllFirst: true}, 1), wantCode: codes.Internal})
	add(testCase{
		name:     "client-stream/two-responses",
		call:     clientStreamCall(&difftestv1.Scenario{ReadAllFirst: true, Responses: 2}, 1),
		wantCode: codes.Internal,
	})
	return cs
}

func (tc testCase) run(t *testing.T, c client) outcome {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if tc.timeout > 0 {
		var cancelTimeout context.CancelFunc
		ctx, cancelTimeout = context.WithTimeout(ctx, tc.timeout)
		defer cancelTimeout()
	}
	if tc.cancelAfter > 0 {
		var cancelCall context.CancelFunc
		ctx, cancelCall = context.WithCancel(ctx)
		defer time.AfterFunc(tc.cancelAfter, cancelCall).Stop()
		defer cancelCall()
	}
	return normalize(tc.call(ctx, c))
}

// normalize makes empty and missing fields compare equal.
func normalize(o outcome) outcome {
	if len(o.Details) == 0 {
		o.Details = nil
	}
	if len(o.Header) == 0 {
		o.Header = nil
	}
	if len(o.Trailer) == 0 {
		o.Trailer = nil
	}
	for i := range o.Responses {
		if len(o.Responses[i].Payload) == 0 {
			o.Responses[i].Payload = nil
		}
	}
	if len(o.Responses) == 0 {
		o.Responses = nil
	}
	return o
}

// TestDifferential runs every case on grpc-go over HTTP/2 and on
// quic-rpc-go over QUIC and over mem, and requires the same outcome
// (TST-5, the first gate of phase 1).
func TestDifferential(t *testing.T) {
	all := stacks(t)
	ref := all[0]
	for _, tc := range cases() {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.run(t, ref.client)
			if want.Code != uint32(tc.wantCode) || len(want.Responses) != tc.wantResponses {
				t.Fatalf("reference %s: %v; want code %v and %d responses", ref.name, want, tc.wantCode, tc.wantResponses)
			}
			for _, s := range all[1:] {
				got := tc.run(t, s.client)
				if tc.codeOnly {
					got.Message, want.Message = "", ""
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s differs from %s:\n got: %v\nwant: %v", s.name, ref.name, got, want)
				}
			}
		})
	}
}

// TestCaseNamesAreUnique keeps subtests addressable with -run.
func TestCaseNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range cases() {
		if seen[tc.name] || strings.ContainsAny(tc.name, " ") {
			t.Errorf("case name %q is duplicated or contains spaces", tc.name)
		}
		seen[tc.name] = true
	}
}
