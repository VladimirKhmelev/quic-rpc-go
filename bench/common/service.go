package common

import (
	"context"
	"errors"
	"fmt"
	"io"

	benchv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1/benchv1quic"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	rpcstatus "github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

// MaxResponse is the default maximum response payload: 4 MiB less room for
// protobuf fields. Explicit message limits can raise this bound.
const MaxResponse = 4<<20 - 64

// responses holds the shared zero-filled payloads. Marshaling copies them, so the
// server allocates nothing but the message itself.
type responses struct{ zeros []byte }

func newResponses(l MessageLimits) responses { return responses{zeros: make([]byte, l.MaxPayload())} }

// response returns the payload of a response of size bytes.
func (r responses) response(size uint32) ([]byte, error) {
	if uint64(size) > uint64(len(r.zeros)) {
		return nil, fmt.Errorf("response_size above %d", len(r.zeros))
	}
	return r.zeros[:size], nil
}

// echo sends back every message of a stream until the client's FIN.
func echo[S interface {
	Recv() (*benchv1.EchoMessage, error)
	Send(*benchv1.EchoMessage) error
}](s S) error {
	for {
		m, err := s.Recv()
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

// quicService serves the service with quic-rpc-go.
type quicService struct {
	responses
	benchv1quic.UnimplementedBenchServer
}

func (s quicService) Call(_ context.Context, req *benchv1.CallRequest) (*benchv1.CallResponse, error) {
	payload, err := s.response(req.GetResponseSize())
	if err != nil {
		return nil, rpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	return &benchv1.CallResponse{Payload: payload}, nil
}

func (quicService) Echo(s rpc.BidiStreamingServer[benchv1.EchoMessage, benchv1.EchoMessage]) error {
	return echo(s)
}

func (s quicService) Bulk(req *benchv1.BulkRequest, stream rpc.ServerStreamingServer[benchv1.BulkChunk]) error {
	err := s.sendBulk(stream.Context(), req, stream.Send)
	if e, ok := errors.AsType[*StatusError](err); ok {
		return rpcstatus.Error(codes.Code(e.Code), e.Message)
	}
	return err
}
