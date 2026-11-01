package common

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/baseline/h2"
	"github.com/VladimirKhmelev/quic-rpc-go/bench/baseline/h2pool"
	benchv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1"
)

// grpcClient is a client of grpc-go over HTTP/2, on one connection or a pool.
type grpcClient struct {
	cc interface {
		grpc.ClientConnInterface
		Close() error
	}
	bench   benchv1.BenchClient
	connect func(context.Context) error
}

func dialGRPC(addr string, tlsConf *tls.Config) (*grpcClient, error) {
	cc, err := h2.Dial(addr, tlsConf)
	if err != nil {
		return nil, err
	}
	return &grpcClient{cc: cc, bench: benchv1.NewBenchClient(cc), connect: func(ctx context.Context) error { return h2.Connect(ctx, cc) }}, nil
}

func dialGRPCPool(addr string, tlsConf *tls.Config, size int) (*grpcClient, error) {
	p, err := h2pool.Dial(addr, tlsConf, size)
	if err != nil {
		return nil, err
	}
	return &grpcClient{cc: p, bench: benchv1.NewBenchClient(p), connect: p.Connect}, nil
}

// Connect waits until grpc-go reports the connection ready: the TCP and
// TLS handshakes are done and the server's HTTP/2 settings have arrived
// on every member. Pool members connect in parallel.
func (c *grpcClient) Connect(ctx context.Context) error {
	return grpcError(c.connect(ctx))
}

func (c *grpcClient) Call(ctx context.Context, payload []byte, respSize int) (int, error) {
	size, err := checkSize(respSize)
	if err != nil {
		return 0, err
	}
	resp, err := c.bench.Call(ctx, &benchv1.CallRequest{Payload: payload, ResponseSize: size})
	if err != nil {
		return 0, grpcError(err)
	}
	return len(resp.GetPayload()), nil
}

func (c *grpcClient) Echo(ctx context.Context) (EchoStream, error) {
	s, err := c.bench.Echo(ctx)
	if err != nil {
		return nil, grpcError(err)
	}
	return grpcEcho{s}, nil
}

func (c *grpcClient) Close() error { return grpcError(c.cc.Close()) }

type grpcEcho struct {
	s grpc.BidiStreamingClient[benchv1.EchoMessage, benchv1.EchoMessage]
}

func (e grpcEcho) Send(seq uint64, payload []byte) error {
	return grpcError(e.s.Send(&benchv1.EchoMessage{Seq: seq, Payload: payload}))
}

func (e grpcEcho) Recv() (uint64, int, error) {
	m, err := e.s.Recv()
	if err != nil {
		return 0, 0, grpcError(err)
	}
	return m.GetSeq(), len(m.GetPayload()), nil
}

func (e grpcEcho) CloseSend() error { return grpcError(e.s.CloseSend()) }

// grpcError turns an error of grpc-go into a StatusError; io.EOF passes
// through.
func grpcError(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	s := status.Convert(err)
	return &StatusError{Code: uint32(s.Code()), Message: s.Message()}
}

// grpcService serves the service with grpc-go.
type grpcService struct {
	benchv1.UnimplementedBenchServer
}

func (grpcService) Call(_ context.Context, req *benchv1.CallRequest) (*benchv1.CallResponse, error) {
	payload, err := response(req.GetResponseSize())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &benchv1.CallResponse{Payload: payload}, nil
}

func (grpcService) Echo(s grpc.BidiStreamingServer[benchv1.EchoMessage, benchv1.EchoMessage]) error {
	return echo(s)
}

type grpcServer struct {
	srv  *grpc.Server
	addr net.Addr
}

func listenGRPC(addr string, tlsConf *tls.Config) (*grpcServer, error) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	s := h2.NewServer(tlsConf)
	benchv1.RegisterBenchServer(s, grpcService{})
	go func() { _ = s.Serve(l) }()
	return &grpcServer{srv: s, addr: l.Addr()}, nil
}

func (s *grpcServer) Addr() net.Addr { return s.addr }

func (s *grpcServer) Stop() { s.srv.Stop() }
