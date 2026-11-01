package difftest_test

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/baseline/httpgrpc"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/difftest"
	difftestv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/difftest/v1"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/gen/difftest/v1/difftestv1connect"
)

type connectClient struct{ client *connect.Client }

// These are native connect-go v2.0.2 responses to malformed calls, not
// application errors. Keep the differences explicit and check exact messages;
// ordinary RPC outcomes still have to match grpc-go in every field.
func connectExpected(name string, expected outcome) outcome {
	switch name {
	case "unary/unknown-method":
		expected.Message = `procedure "/difftest.v1.DiffTest/Missing" not registered`
	case "unary/unknown-service":
		expected.Message = "HTTP status 404 Not Found"
	case "server-stream/no-request":
		expected.Code, expected.Message = 12, "unary request has zero messages"
	case "server-stream/two-requests":
		expected.Code, expected.Message = 12, "unary request has multiple messages"
	case "client-stream/no-response":
		expected.Code, expected.Message = 12, "unary stream has zero messages"
	case "client-stream/two-responses":
		expected.Code, expected.Message = 12, "unary stream has multiple messages"
	}
	return expected
}

func connectSpec(method string, kind connect.StreamType) connect.Spec {
	name := method[strings.LastIndexByte(method, '/')+1:]
	schema := difftestv1.File_difftest_v1_difftest_proto.Services().ByName("DiffTest").Methods().ByName(protoreflect.Name(name))
	if schema == nil {
		schema = difftestv1.File_difftest_v1_difftest_proto.Services().ByName("DiffTest").Methods().Get(0)
	}
	return connect.Spec{Procedure: method, StreamType: kind, Schema: schema}
}

func connectOutcome(err error, info *connect.CallInfo) outcome {
	o := outcome{Header: onlyX(difftest.DecodeConnectHeader(info.ResponseHeader())), Trailer: onlyX(difftest.DecodeConnectHeader(info.ResponseTrailer()))}
	if err == nil || errors.Is(err, io.EOF) {
		return o
	}
	if ce, ok := errors.AsType[*connect.Error](err); ok {
		o.Code, o.Message = uint32(ce.Code()), ce.Message()
		for _, d := range ce.Details() {
			msg, err := connectproto.UnmarshalErrorDetail(d)
			if err == nil {
				o.Details = append(o.Details, detailTexts([]any{msg})...)
			}
		}
	} else {
		o.Code, o.Message = uint32(connect.CodeUnknown), err.Error()
		if errors.Is(err, context.Canceled) {
			o.Code = uint32(connect.CodeCanceled)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			o.Code = uint32(connect.CodeDeadlineExceeded)
		}
	}
	return o
}

func (c connectClient) unary(ctx context.Context, md map[string][]string, req *difftestv1.Request) outcome {
	ctx, info := connect.NewClientContext(ctx)
	difftest.SetConnectHeader(info.RequestHeader(), md)
	res, err := difftestv1connect.NewDiffTestClient(c.client).Unary(ctx, req)
	o := connectOutcome(err, info)
	if err == nil {
		o.Responses = []response{{res.Seq, res.Payload}}
	}
	return o
}

func (c connectClient) invoke(ctx context.Context, method string) outcome {
	ctx, info := connect.NewClientContext(ctx)
	err := c.client.CallUnary(ctx, connectSpec(method, connect.StreamTypeUnary), &difftestv1.Request{}, &difftestv1.Response{})
	return connectOutcome(err, info)
}

type connectReceive struct{ connect.ClientStream }

func (s connectReceive) Send(req *difftestv1.Request) error { return s.ClientStream.Send(req) }
func (s connectReceive) Recv() (*difftestv1.Response, error) {
	res := new(difftestv1.Response)
	err := s.Receive(res)
	return res, err
}

func (c connectClient) bidi(ctx context.Context, mode bidiMode, reqs []*difftestv1.Request) outcome {
	ctx, info := connect.NewClientContext(ctx)
	s, err := c.client.CallClientStream(ctx, connectSpec("/difftest.v1.DiffTest/Bidi", connect.StreamTypeBidi))
	if err != nil {
		return connectOutcome(err, info)
	}
	defer func() { _ = s.Close() }()
	responses, err := drive(connectReceive{s}, mode, reqs)
	o := connectOutcome(err, info)
	o.Responses = responses
	return o
}

func (c connectClient) serverStream(ctx context.Context, md map[string][]string, req *difftestv1.Request) outcome {
	ctx, info := connect.NewClientContext(ctx)
	difftest.SetConnectHeader(info.RequestHeader(), md)
	s, err := c.client.CallServerStream(ctx, connectSpec("/difftest.v1.DiffTest/ServerStream", connect.StreamTypeServer), req)
	if err != nil {
		return connectOutcome(err, info)
	}
	defer func() { _ = s.Close() }()
	responses, err := recvAll(connectReceive{s}.Recv)
	o := connectOutcome(err, info)
	o.Responses = responses
	return o
}

func (c connectClient) clientStream(ctx context.Context, reqs []*difftestv1.Request) outcome {
	ctx, info := connect.NewClientContext(ctx)
	s, err := c.client.CallClientStream(ctx, connectSpec("/difftest.v1.DiffTest/ClientStream", connect.StreamTypeClient))
	if err != nil {
		return connectOutcome(err, info)
	}
	defer func() { _ = s.Close() }()
	sendAllReqs(s.Send, reqs)
	_ = s.CloseSend()
	res, err := (connectReceive{s}).Recv()
	o := connectOutcome(err, info)
	if err == nil {
		o.Responses = []response{{res.Seq, res.Payload}}
	}
	return o
}

func (c connectClient) raw(ctx context.Context, method string, reqs []*difftestv1.Request) outcome {
	ctx, info := connect.NewClientContext(ctx)
	s, err := c.client.CallClientStream(ctx, connectSpec(method, connect.StreamTypeBidi))
	if err != nil {
		return connectOutcome(err, info)
	}
	defer func() { _ = s.Close() }()
	sendAllReqs(s.Send, reqs)
	_ = s.CloseSend()
	responses, err := recvAll(connectReceive{s}.Recv)
	o := connectOutcome(err, info)
	o.Responses = responses
	return o
}

func serveConnect(t *testing.T, serverTLS, clientTLS *tls.Config, version int) connectClient {
	t.Helper()
	s := connect.NewServer()
	difftest.RegisterConnect(s)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, s, connecthttp.WithCompressors(), connecthttp.WithReadMaxBytes(4<<20))
	srv, err := httpgrpc.Listen("127.0.0.1:0", serverTLS, version, mux)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Stop)
	c, err := httpgrpc.NewClient(srv.Addr().String(), clientTLS, version)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	client := connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: c}, "https://"+srv.Addr().String(), connecthttp.WithGRPC(), connecthttp.WithCompressors(), connecthttp.WithReadMaxBytes(4<<20)))
	ctx, info := connect.NewClientContext(t.Context())
	if _, err := difftestv1connect.NewDiffTestClient(client).Unary(ctx, &difftestv1.Request{}); err != nil {
		t.Fatal(err)
	}
	httpInfo, ok := connecthttp.ClientInfoForContext(ctx)
	if !ok || httpInfo.ResponseProto() != fmt.Sprintf("HTTP/%d.0", version) || info.Protocol != "grpc" || info.ResponseEncoding != "identity" {
		t.Fatalf("wrong wire protocol: HTTP info=%v, RPC protocol=%s, compression=%s", httpInfo, info.Protocol, info.ResponseEncoding)
	}
	return connectClient{client}
}
