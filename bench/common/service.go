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

// MaxResponse is the largest response Call can ask for: the 4 MiB message
// limit of both libraries, less room for the framing of the message.
const MaxResponse = 4<<20 - 64

// zeros backs the payloads of responses. Marshaling copies it, so the
// server allocates nothing but the message itself.
var zeros = make([]byte, MaxResponse)

var errTooLarge = fmt.Errorf("response_size above %d", MaxResponse)

// response returns the payload of a response of size bytes.
func response(size uint32) ([]byte, error) {
	if size > MaxResponse {
		return nil, errTooLarge
	}
	return zeros[:size], nil
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
	benchv1quic.UnimplementedBenchServer
}

func (quicService) Call(_ context.Context, req *benchv1.CallRequest) (*benchv1.CallResponse, error) {
	payload, err := response(req.GetResponseSize())
	if err != nil {
		return nil, rpcstatus.Error(codes.InvalidArgument, err.Error())
	}
	return &benchv1.CallResponse{Payload: payload}, nil
}

func (quicService) Echo(s rpc.BidiStreamingServer[benchv1.EchoMessage, benchv1.EchoMessage]) error {
	return echo(s)
}
