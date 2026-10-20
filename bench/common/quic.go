package common

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync"

	benchv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1/benchv1quic"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	rpcstatus "github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
	rpcquic "github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/quic"
)

// rpcClient is a client of quic-rpc-go. Its ClientConn dials through it,
// so that Connect can set up the connection before the first call.
type rpcClient struct {
	dialer transport.Dialer
	cc     *rpc.ClientConn
	bench  benchv1quic.BenchClient

	mu    sync.Mutex
	ready transport.Conn // set up by Connect, not yet taken by cc
}

// NewRPCClient returns a client of quic-rpc-go over any transport, such as
// the in-memory one of tests. It does not connect yet.
func NewRPCClient(d transport.Dialer) Client {
	c := &rpcClient{dialer: d}
	c.cc = rpc.NewClient(c)
	c.bench = benchv1quic.NewBenchClient(c.cc)
	return c
}

func dialQUIC(addr string, tlsConf *tls.Config) Client {
	return NewRPCClient(&rpcquic.Dialer{Addr: addr, TLSConfig: tlsConf})
}

// Dial hands the ClientConn the connection Connect set up, or dials anew.
func (c *rpcClient) Dial(ctx context.Context) (transport.Conn, error) {
	c.mu.Lock()
	conn := c.ready
	c.ready = nil
	c.mu.Unlock()
	if conn != nil && conn.Context().Err() == nil {
		return conn, nil
	}
	return c.dialer.Dial(ctx)
}

func (c *rpcClient) Connect(ctx context.Context) error {
	conn, err := c.dialer.Dial(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return quicError(rpcstatus.FromContextError(ctx.Err()).Err())
		}
		return &StatusError{Code: uint32(codes.Unavailable), Message: err.Error()}
	}
	c.mu.Lock()
	old := c.ready
	c.ready = conn
	c.mu.Unlock()
	if old != nil {
		_ = old.CloseWithError(transport.NoError, "replaced by a new connection")
	}
	return nil
}

func (c *rpcClient) Call(ctx context.Context, payload []byte, respSize int) (int, error) {
	size, err := checkSize(respSize)
	if err != nil {
		return 0, err
	}
	resp, err := c.bench.Call(ctx, &benchv1.CallRequest{Payload: payload, ResponseSize: size})
	if err != nil {
		return 0, quicError(err)
	}
	return len(resp.GetPayload()), nil
}

func (c *rpcClient) Echo(ctx context.Context) (EchoStream, error) {
	s, err := c.bench.Echo(ctx)
	if err != nil {
		return nil, quicError(err)
	}
	return quicEcho{s}, nil
}

func (c *rpcClient) Close() error {
	c.mu.Lock()
	conn := c.ready
	c.ready = nil
	c.mu.Unlock()
	if conn != nil {
		_ = conn.CloseWithError(transport.NoError, "client closed")
	}
	return quicError(c.cc.Close())
}

type quicEcho struct {
	s rpc.BidiStreamingClient[benchv1.EchoMessage, benchv1.EchoMessage]
}

func (e quicEcho) Send(seq uint64, payload []byte) error {
	return quicError(e.s.Send(&benchv1.EchoMessage{Seq: seq, Payload: payload}))
}

func (e quicEcho) Recv() (uint64, int, error) {
	m, err := e.s.Recv()
	if err != nil {
		return 0, 0, quicError(err)
	}
	return m.GetSeq(), len(m.GetPayload()), nil
}

func (e quicEcho) CloseSend() error { return quicError(e.s.CloseSend()) }

// quicError turns an error of quic-rpc-go into a StatusError; io.EOF, the
// end of a stream, passes through.
func quicError(err error) error {
	if err == nil || errors.Is(err, io.EOF) {
		return err
	}
	s := rpcstatus.Convert(err)
	return &StatusError{Code: uint32(s.Code()), Message: s.Message()}
}

// checkSize checks a response size on the client, with the error the server
// would return.
func checkSize(respSize int) (uint32, error) {
	if respSize < 0 || respSize > MaxResponse {
		return 0, &StatusError{Code: uint32(codes.InvalidArgument), Message: errTooLarge.Error()}
	}
	return uint32(respSize), nil
}

type quicServer struct {
	srv  *rpc.Server
	addr net.Addr
}

func listenQUIC(addr string, tlsConf *tls.Config) (*quicServer, error) {
	l, err := rpcquic.Listen(addr, tlsConf, nil)
	if err != nil {
		return nil, err
	}
	s := rpc.NewServer()
	benchv1quic.RegisterBenchServer(s, quicService{})
	go func() { _ = s.Serve(l) }()
	return &quicServer{srv: s, addr: l.Addr()}, nil
}

func (s *quicServer) Addr() net.Addr { return s.addr }

func (s *quicServer) Stop() { s.srv.Stop() }
