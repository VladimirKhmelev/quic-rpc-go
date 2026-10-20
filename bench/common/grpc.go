package common

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/status"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/baseline/h2"
	benchv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1"
)

// grpcClient is a client of grpc-go over HTTP/2, on one connection.
type grpcClient struct {
	cc    *grpc.ClientConn
	bench benchv1.BenchClient
}

func dialGRPC(addr string, tlsConf *tls.Config) (*grpcClient, error) {
	cc, err := h2.Dial(addr, tlsConf)
	if err != nil {
		return nil, err
	}
	return &grpcClient{cc: cc, bench: benchv1.NewBenchClient(cc)}, nil
}

// Connect waits until grpc-go reports the connection ready: the TCP and
// TLS handshakes are done and the server's HTTP/2 settings have arrived.
func (c *grpcClient) Connect(ctx context.Context) error {
	c.cc.Connect()
	for {
		switch s := c.cc.GetState(); s {
		case connectivity.Ready:
			return nil
		case connectivity.TransientFailure, connectivity.Shutdown:
			return &StatusError{Code: uint32(codes.Unavailable), Message: "connection " + s.String()}
		default:
			if !c.cc.WaitForStateChange(ctx, s) {
				return grpcError(status.FromContextError(ctx.Err()).Err())
			}
		}
	}
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
