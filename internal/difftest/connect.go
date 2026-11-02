package difftest

import (
	"context"
	"strings"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectproto"
	"google.golang.org/protobuf/reflect/protoreflect"

	difftestv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/difftest/v1"
)

// RegisterConnect registers the same scenario logic on connect-go. Raw handlers
// let the cardinality cases deliberately send too many or too few responses.
func RegisterConnect(server *connect.Server) {
	svc := difftestv1.File_difftest_v1_difftest_proto.Services().ByName("DiffTest")
	for _, item := range []struct {
		name string
		kind connect.StreamType
	}{
		{"Unary", connect.StreamTypeUnary}, {"Bidi", connect.StreamTypeBidi},
		{"ServerStream", connect.StreamTypeServer}, {"ClientStream", connect.StreamTypeClient},
	} {
		server.Register(connect.Method{
			Spec: connect.Spec{Procedure: "/difftest.v1.DiffTest/" + item.name, StreamType: item.kind, Schema: svc.Methods().ByName(protoreflect.Name(item.name))},
			Handler: func(ctx context.Context, _ connect.Spec, stream connect.ServerStream) error {
				info, _ := connect.CallInfoForServerContext(ctx)
				side := side{
					incoming:  DecodeConnectHeader(info.RequestHeader()),
					setHeader: func(md map[string][]string) error { SetConnectHeader(info.ResponseHeader(), md); return nil },
					sendHeader: func(md map[string][]string) error {
						SetConnectHeader(info.ResponseHeader(), md)
						return stream.SendHeaders()
					},
					setTrailer: func(md map[string][]string) error { SetConnectHeader(info.ResponseTrailer(), md); return nil },
					status:     connectStatus,
				}
				s := connectScenarioStream{ServerStream: stream, ctx: ctx}
				switch item.name {
				case "Unary":
					req, err := s.Recv()
					if err != nil {
						return err
					}
					res, err := unary(ctx, side, req)
					if err != nil {
						return err
					}
					return s.Send(res)
				case "Bidi":
					return bidi(side, s)
				case "ServerStream":
					req, err := s.Recv()
					if err != nil {
						return err
					}
					return serverStreaming(side, req, s)
				default:
					return clientStreaming(side, s)
				}
			},
		})
	}
}

type connectScenarioStream struct {
	connect.ServerStream
	ctx context.Context
}

func (s connectScenarioStream) Context() context.Context { return s.ctx }
func (s connectScenarioStream) Recv() (*difftestv1.Request, error) {
	req := new(difftestv1.Request)
	err := s.Receive(req)
	return req, err
}
func (s connectScenarioStream) Send(res *difftestv1.Response) error         { return s.ServerStream.Send(res) }
func (s connectScenarioStream) SendAndClose(res *difftestv1.Response) error { return s.Send(res) }

func connectStatus(code uint32, msg string, details []*difftestv1.Detail) error {
	err := connect.NewError(connect.Code(code), msg)
	for _, detail := range details {
		d, e := connectproto.NewErrorDetail(detail)
		if e != nil {
			return e
		}
		err = err.WithDetail(d)
	}
	return err
}

// SetConnectHeader transfers gRPC-style raw binary values to HTTP headers.
func SetConnectHeader(header *connect.Header, md map[string][]string) {
	for key, values := range md {
		for _, v := range values {
			if strings.HasSuffix(key, "-bin") {
				v = connect.EncodeBinaryHeader([]byte(v))
			}
			header.Add(key, v)
		}
	}
}

// DecodeConnectHeader returns lower-case keys and decoded binary values.
func DecodeConnectHeader(header *connect.Header) map[string][]string {
	md := make(map[string][]string)
	for key, values := range header.All() {
		key = strings.ToLower(key)
		for _, v := range values {
			if strings.HasSuffix(key, "-bin") {
				if b, err := connect.DecodeBinaryHeader(v); err == nil {
					v = string(b)
				}
			}
			md[key] = append(md[key], v)
		}
	}
	return md
}
