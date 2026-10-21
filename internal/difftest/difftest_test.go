package difftest_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	grpcmetadata "google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/baseline/h2"
	"github.com/VladimirKhmelev/quic-rpc-go/grpcbridge"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/certs"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/difftest"
	difftestv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/difftest/v1"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/gen/difftest/v1/difftestv1quic"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/mem"
	rpcquic "github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/quic"
)

// outcome is everything a client sees of a call.
type outcome struct {
	Code      uint32
	Message   string
	Details   []string
	Header    map[string][]string // only keys starting with "x-"
	Trailer   map[string][]string // only keys starting with "x-"
	Responses []response
}

type response struct {
	Seq     int32
	Payload []byte
}

func (o outcome) String() string {
	var rs []string
	for _, r := range o.Responses {
		rs = append(rs, fmt.Sprintf("{seq %d, %d bytes}", r.Seq, len(r.Payload)))
	}
	return fmt.Sprintf("code %d %q, details %q, header %q, trailer %q, responses [%s]",
		o.Code, o.Message, o.Details, o.Header, o.Trailer, strings.Join(rs, " "))
}

type bidiMode int

const (
	interleave bidiMode = iota // Send and Recv in turn, then CloseSend
	sendAll                    // Send everything, CloseSend, then Recv
	sendFirst                  // Send the first request only and Recv
)

// client drives calls on one stack.
type client interface {
	unary(ctx context.Context, md map[string][]string, req *difftestv1.Request) outcome
	invoke(ctx context.Context, method string) outcome
	bidi(ctx context.Context, mode bidiMode, reqs []*difftestv1.Request) outcome
}

// clientStream is the client side of Bidi on both stacks.
type clientStream interface {
	Send(*difftestv1.Request) error
	Recv() (*difftestv1.Response, error)
	CloseSend() error
}

// drive runs a Bidi call and returns the responses and the error that ended it.
func drive(s clientStream, mode bidiMode, reqs []*difftestv1.Request) ([]response, error) {
	var out []response
	switch mode {
	case interleave:
		for _, r := range reqs {
			if err := s.Send(r); err != nil {
				break // the call is over; Recv reports why
			}
			resp, err := s.Recv()
			if err != nil {
				return out, err
			}
			out = append(out, response{resp.GetSeq(), resp.GetPayload()})
		}
	case sendAll:
		for _, r := range reqs {
			_ = s.Send(r) // io.EOF once the server ended the call
		}
	case sendFirst:
		_ = s.Send(reqs[0])
	}
	if mode != sendFirst {
		_ = s.CloseSend()
	}
	for {
		resp, err := s.Recv()
		if err != nil {
			return out, err
		}
		out = append(out, response{resp.GetSeq(), resp.GetPayload()})
	}
}

func onlyX(md map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range md {
		if strings.HasPrefix(k, "x-") {
			out[k] = v
		}
	}
	return out
}

func detailTexts(details []any) []string {
	var out []string
	for _, d := range details {
		if dt, ok := d.(*difftestv1.Detail); ok {
			out = append(out, dt.GetText())
		} else {
			out = append(out, fmt.Sprint(d))
		}
	}
	return out
}

// grpcClient calls through code generated for grpc-go, over grpc-go itself
// or over grpcbridge.
type grpcClient struct {
	conn grpc.ClientConnInterface
	c    difftestv1.DiffTestClient
}

func grpcOutcome(err error) outcome {
	if err == nil || errors.Is(err, io.EOF) {
		return outcome{}
	}
	st := grpcstatus.Convert(err)
	return outcome{Code: uint32(st.Code()), Message: st.Message(), Details: detailTexts(st.Details())}
}

func (g grpcClient) unary(ctx context.Context, md map[string][]string, req *difftestv1.Request) outcome {
	ctx = grpcmetadata.NewOutgoingContext(ctx, md)
	var header, trailer grpcmetadata.MD
	resp, err := g.c.Unary(ctx, req, grpc.Header(&header), grpc.Trailer(&trailer))
	o := grpcOutcome(err)
	o.Header, o.Trailer = onlyX(header), onlyX(trailer)
	if err == nil {
		o.Responses = []response{{resp.GetSeq(), resp.GetPayload()}}
	}
	return o
}

func (g grpcClient) invoke(ctx context.Context, method string) outcome {
	return grpcOutcome(g.conn.Invoke(ctx, method, &difftestv1.Request{}, new(difftestv1.Response)))
}

func (g grpcClient) bidi(ctx context.Context, mode bidiMode, reqs []*difftestv1.Request) outcome {
	stream, err := g.c.Bidi(ctx)
	if err != nil {
		return grpcOutcome(err)
	}
	responses, err := drive(stream, mode, reqs)
	o := grpcOutcome(err)
	header, _ := stream.Header()
	o.Header, o.Trailer, o.Responses = onlyX(header), onlyX(stream.Trailer()), responses
	return o
}

type quicClient struct {
	cc *rpc.ClientConn
	c  difftestv1quic.DiffTestClient
}

func quicOutcome(err error) outcome {
	if err == nil || errors.Is(err, io.EOF) {
		return outcome{}
	}
	st := status.Convert(err)
	return outcome{Code: uint32(st.Code()), Message: st.Message(), Details: detailTexts(st.Details())}
}

func (q quicClient) unary(ctx context.Context, md map[string][]string, req *difftestv1.Request) outcome {
	ctx = metadata.NewOutgoingContext(ctx, md)
	var header, trailer metadata.MD
	resp, err := q.c.Unary(ctx, req, rpc.Header(&header), rpc.Trailer(&trailer))
	o := quicOutcome(err)
	o.Header, o.Trailer = onlyX(header), onlyX(trailer)
	if err == nil {
		o.Responses = []response{{resp.GetSeq(), resp.GetPayload()}}
	}
	return o
}

func (q quicClient) invoke(ctx context.Context, method string) outcome {
	return quicOutcome(q.cc.Invoke(ctx, method, &difftestv1.Request{}, new(difftestv1.Response)))
}

func (q quicClient) bidi(ctx context.Context, mode bidiMode, reqs []*difftestv1.Request) outcome {
	stream, err := q.c.Bidi(ctx)
	if err != nil {
		return quicOutcome(err)
	}
	responses, err := drive(stream, mode, reqs)
	o := quicOutcome(err)
	header, _ := stream.Header()
	o.Header, o.Trailer, o.Responses = onlyX(header), onlyX(stream.Trailer()), responses
	return o
}

type stack struct {
	name   string
	client client
}

// stacks starts the reference (grpc-go over HTTP/2 and TLS) and the stacks
// under test, all serving the same scenario logic: quic-rpc-go over QUIC
// and over mem, and code generated for grpc-go over quic-rpc-go through
// grpcbridge, again over QUIC and over mem. The reference comes first.
func stacks(t *testing.T) []stack {
	t.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true")
	pair, err := certs.New("localhost", "127.0.0.1")
	if err != nil {
		t.Fatalf("certs: %v", err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	gsrv := h2.NewServer(pair.Server)
	difftestv1.RegisterDiffTestServer(gsrv, difftest.GRPCServer{})
	go func() { _ = gsrv.Serve(lis) }()
	t.Cleanup(gsrv.Stop)
	conn, err := h2.Dial(lis.Addr().String(), pair.Client)
	if err != nil {
		t.Fatalf("h2.Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	listenQUIC := func() *rpcquic.Listener {
		l, err := rpcquic.Listen("127.0.0.1:0", pair.Server, nil)
		if err != nil {
			t.Fatalf("rpcquic.Listen: %v", err)
		}
		return l
	}
	ql, bql := listenQUIC(), listenQUIC()
	ml, bml := mem.Listen(mem.Options{}), mem.Listen(mem.Options{})

	return []stack{
		{"grpc-h2", grpcClient{conn, difftestv1.NewDiffTestClient(conn)}},
		{"rpc-quic", serveQUIC(t, ql, &rpcquic.Dialer{Addr: ql.Addr().String(), TLSConfig: pair.Client})},
		{"rpc-mem", serveQUIC(t, ml, ml)},
		{"bridge-quic", serveBridge(t, bql, &rpcquic.Dialer{Addr: bql.Addr().String(), TLSConfig: pair.Client})},
		{"bridge-mem", serveBridge(t, bml, bml)},
	}
}

// serveBridge serves the grpc-go implementation of the scenarios through
// grpcbridge and calls it with the client generated for grpc-go.
func serveBridge(t *testing.T, l transport.Listener, d transport.Dialer) grpcClient {
	t.Helper()
	srv := grpcbridge.NewServer()
	difftestv1.RegisterDiffTestServer(srv, difftest.GRPCServer{})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	cc := grpcbridge.NewClient(d)
	t.Cleanup(func() { _ = cc.Close() })
	return grpcClient{cc, difftestv1.NewDiffTestClient(cc)}
}

func serveQUIC(t *testing.T, l transport.Listener, d transport.Dialer) quicClient {
	t.Helper()
	srv := rpc.NewServer()
	difftestv1quic.RegisterDiffTestServer(srv, difftest.QUICServer{})
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)
	cc := rpc.NewClient(d)
	t.Cleanup(func() { _ = cc.Close() })
	return quicClient{cc, difftestv1quic.NewDiffTestClient(cc)}
}
