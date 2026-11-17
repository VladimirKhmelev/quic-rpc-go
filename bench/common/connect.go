package common

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"mime"
	"net/http"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/quic-go/quic-go/http3"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/baseline/httpgrpc"
	benchv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1/benchv1connect"
)

func httpVersion(t Transport) int {
	if t == GRPCH3 {
		return 3
	}
	return 2
}

type connectClient struct {
	http   *httpgrpc.Client
	bench  benchv1connect.BenchClient
	limits MessageLimits
}

func dialConnect(t Transport, addr string, tlsConf *tls.Config, limits MessageLimits) (*connectClient, error) {
	hc, err := httpgrpc.NewClient(addr, tlsConf, httpVersion(t))
	if err != nil {
		return nil, err
	}
	client := connect.NewClient(connecthttp.NewTransport(&http.Client{Transport: hc}, "https://"+addr,
		connecthttp.WithGRPC(), connecthttp.WithCompressors(), connecthttp.WithReadMaxBytes(limits.MaxRecvMsgBytes), connecthttp.WithSendMaxBytes(limits.MaxSendMsgBytes)))
	return &connectClient{limits: limits, http: hc, bench: benchv1connect.NewBenchClient(client)}, nil
}

func (c *connectClient) Connect(ctx context.Context) error { return c.result(ctx, c.http.Connect(ctx)) }
func (c *connectClient) Close() error                      { return c.http.Close() }

func (c *connectClient) Call(ctx context.Context, payload []byte, respSize int) (int, error) {
	size, err := checkSize(respSize, c.limits.MaxPayload())
	if err != nil {
		return 0, err
	}
	res, err := c.bench.Call(ctx, &benchv1.CallRequest{Payload: payload, ResponseSize: size})
	if err != nil {
		return 0, c.result(ctx, err)
	}
	return len(res.Payload), nil
}

func (c *connectClient) Echo(ctx context.Context) (EchoStream, error) {
	s, err := c.bench.Echo(ctx)
	if err != nil {
		return nil, c.result(ctx, err)
	}
	if err := s.SendHeaders(); err != nil {
		_ = s.Close()
		return nil, c.result(ctx, err)
	}
	return connectEcho{stream: s, ctx: ctx, client: c}, nil
}

func (c *connectClient) result(ctx context.Context, err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	code := connect.CodeOf(err)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		code = connect.CodeDeadlineExceeded
	case ctx.Err() != nil || c.http.IsClosed():
		code = connect.CodeCanceled
	case code == connect.CodeInternal:
		// connect-go wraps a connection closed mid-envelope as Internal.
		// H3_NO_ERROR is a transport shutdown, not a peer RPC verdict.
		ce, _ := errors.AsType[*connect.Error](err)
		if he, ok := errors.AsType[*http3.Error](err); ok && he.ErrorCode == http3.ErrCodeNoError && ce != nil && !ce.IsRemote() {
			code = connect.CodeUnavailable
		}
	case code == connect.CodeUnknown || code == connect.CodeCanceled:
		// Preserve peer verdicts. With a live call context and client,
		// local cancellation (including connect-go's EPIPE/ECONNRESET
		// mapping) means a transport failure, not a caller cancellation.
		if ce, ok := errors.AsType[*connect.Error](err); !ok || !ce.IsRemote() {
			code = connect.CodeUnavailable
		}
	}
	message := err.Error()
	if ce, ok := errors.AsType[*connect.Error](err); ok {
		message = ce.Message()
	}
	return &StatusError{Code: uint32(code), Message: message}
}

type connectEcho struct {
	stream benchv1connect.BenchEchoClientStream
	ctx    context.Context
	client *connectClient
}

func (e connectEcho) Send(seq uint64, payload []byte) error {
	return e.client.result(e.ctx, e.stream.Send(&benchv1.EchoMessage{Seq: seq, Payload: payload}))
}
func (e connectEcho) CloseSend() error { return e.client.result(e.ctx, e.stream.CloseSend()) }
func (e connectEcho) Recv() (uint64, int, error) {
	m, err := e.stream.Receive()
	if err != nil {
		_ = e.stream.Close()
		return 0, 0, e.client.result(e.ctx, err)
	}
	return m.Seq, len(m.Payload), nil
}

// connectService is exactly the same handler for HTTP/2 and HTTP/3.
type connectService struct{ responses }

func (s connectService) Call(_ context.Context, req *benchv1.CallRequest) (*benchv1.CallResponse, error) {
	payload, err := s.response(req.ResponseSize)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err.Error())
	}
	return &benchv1.CallResponse{Payload: payload}, nil
}

func (connectService) Echo(_ context.Context, s benchv1connect.BenchEchoServerStream) error {
	for {
		m, err := s.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := s.Send(m); err != nil {
			return err
		}
	}
}

func listenConnect(t Transport, addr string, tlsConf *tls.Config, limits MessageLimits) (*httpgrpc.Server, error) {
	srv := connect.NewServer()
	benchv1connect.RegisterBenchHandler(srv, connectService{responses: newResponses(limits)})
	mux := http.NewServeMux()
	connecthttp.Mount(mux, srv, connecthttp.WithCompressors(), connecthttp.WithReadMaxBytes(limits.MaxRecvMsgBytes), connecthttp.WithSendMaxBytes(limits.MaxSendMsgBytes))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || (media != "application/grpc" && media != "application/grpc+proto") {
			http.Error(w, "gRPC required", http.StatusUnsupportedMediaType)
			return
		}
		mux.ServeHTTP(w, r)
	})
	return httpgrpc.Listen(addr, tlsConf, httpVersion(t), handler)
}

func (c *connectClient) Bulk(ctx context.Context, total, chunk int) (BulkStream, error) {
	req, err := bulkRequest(total, chunk, c.limits.MaxPayload())
	if err != nil {
		return nil, err
	}
	stream, err := c.bench.Bulk(ctx, req)
	if err != nil {
		return nil, c.result(ctx, err)
	}
	return bulkReceiver{ctx: ctx, receive: stream.Receive, result: func(err error) error {
		_ = stream.Close()
		return c.result(ctx, err)
	}}, nil
}

func (s connectService) Bulk(ctx context.Context, req *benchv1.BulkRequest, stream benchv1connect.BenchBulkServerStream) error {
	err := s.sendBulk(ctx, req, stream.Send)
	if e, ok := errors.AsType[*StatusError](err); ok {
		return connect.NewError(connect.Code(e.Code), e.Message)
	}
	return err
}
