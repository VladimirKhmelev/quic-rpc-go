// Package difftest runs the same scenarios through grpc-go, quic-rpc-go and
// connect-go and compares what clients see (TST-5). The servers execute
// the scenario logic of this file through small adapters, so any difference
// comes from the stacks, not from the handlers.
package difftest

import (
	"context"
	"errors"
	"io"
	"strings"

	"google.golang.org/grpc"
	grpccodes "google.golang.org/grpc/codes"
	grpcmetadata "google.golang.org/grpc/metadata"
	grpcstatus "google.golang.org/grpc/status"

	difftestv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/difftest/v1"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/gen/difftest/v1/difftestv1quic"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

// side is what the scenario logic needs from the stack serving a call.
type side struct {
	incoming   map[string][]string
	setHeader  func(map[string][]string) error
	sendHeader func(map[string][]string) error
	setTrailer func(map[string][]string) error
	status     func(code uint32, msg string, details []*difftestv1.Detail) error
}

// bidiStream, serverStream and clientStream are the server sides of the
// streaming calls, implemented by generated types or small adapters.
type (
	bidiStream interface {
		Recv() (*difftestv1.Request, error)
		Send(*difftestv1.Response) error
		Context() context.Context
	}
	serverStream interface {
		Send(*difftestv1.Response) error
		Context() context.Context
	}
	clientStream interface {
		Recv() (*difftestv1.Request, error)
		SendAndClose(*difftestv1.Response) error
		Context() context.Context
	}
)

// prepare sets the header and the trailer the scenario asks for.
func (s side) prepare(sc *difftestv1.Scenario) error {
	header := toMD(sc.GetHeader())
	if sc.GetEchoMetadata() {
		for k, v := range s.incoming {
			if strings.HasPrefix(k, "x-") {
				header[k] = append(header[k], v...)
			}
		}
	}
	if sc.GetSendHeaderFirst() {
		if err := s.sendHeader(header); err != nil {
			return err
		}
	} else if len(header) > 0 {
		if err := s.setHeader(header); err != nil {
			return err
		}
	}
	if trailer := toMD(sc.GetTrailer()); len(trailer) > 0 {
		return s.setTrailer(trailer)
	}
	return nil
}

func (s side) end(sc *difftestv1.Scenario) error {
	if sc.GetCode() == 0 {
		return nil
	}
	return s.status(sc.GetCode(), sc.GetMessage(), sc.GetDetails())
}

func unary(ctx context.Context, s side, req *difftestv1.Request) (*difftestv1.Response, error) {
	sc := req.GetScenario()
	if err := s.prepare(sc); err != nil {
		return nil, err
	}
	if sc.GetWaitForCancel() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err := s.end(sc); err != nil {
		return nil, err
	}
	return &difftestv1.Response{Seq: req.GetSeq(), Payload: payload(sc, req.GetPayload())}, nil
}

func bidi(s side, stream bidiStream) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	sc := first.GetScenario()
	if err := s.prepare(sc); err != nil {
		return err
	}
	if sc.GetWaitForCancel() {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	switch {
	case sc.GetEcho():
		for req := first; ; {
			if err := stream.Send(&difftestv1.Response{Seq: req.GetSeq(), Payload: req.GetPayload()}); err != nil {
				return err
			}
			if req, err = stream.Recv(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return err
			}
		}
	default:
		if sc.GetReadAllFirst() {
			for {
				if _, err := stream.Recv(); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					return err
				}
			}
		}
		for i := range sc.GetResponses() {
			if err := stream.Send(&difftestv1.Response{Seq: i, Payload: payload(sc, nil)}); err != nil {
				return err
			}
		}
	}
	return s.end(sc)
}

func serverStreaming(s side, req *difftestv1.Request, stream serverStream) error {
	sc := req.GetScenario()
	if err := s.prepare(sc); err != nil {
		return err
	}
	if sc.GetWaitForCancel() {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	for i := range sc.GetResponses() {
		if err := stream.Send(&difftestv1.Response{Seq: i, Payload: payload(sc, req.GetPayload())}); err != nil {
			return err
		}
	}
	return s.end(sc)
}

func clientStreaming(s side, stream clientStream) error {
	first, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		// No request, so no scenario either.
		return stream.SendAndClose(&difftestv1.Response{})
	}
	if err != nil {
		return err
	}
	sc := first.GetScenario()
	if err := s.prepare(sc); err != nil {
		return err
	}
	if sc.GetWaitForCancel() {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	read := int32(1)
	for sc.GetReadAllFirst() {
		if _, err := stream.Recv(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		read++
	}
	for range sc.GetResponses() {
		if err := stream.SendAndClose(&difftestv1.Response{Seq: read, Payload: payload(sc, nil)}); err != nil {
			return err
		}
	}
	return s.end(sc)
}

// payload returns payload_size bytes of a fixed pattern, or echo if the
// scenario sets no size.
func payload(sc *difftestv1.Scenario, echo []byte) []byte {
	n := sc.GetPayloadSize()
	if n <= 0 {
		return echo
	}
	b := make([]byte, n)
	for i := range b {
		b[i] = byte('a' + i%26)
	}
	return b
}

// toMD turns repeated metadata into a map, keeping repeated keys.
func toMD(md []*difftestv1.Metadata) map[string][]string {
	out := map[string][]string{}
	for _, m := range md {
		out[m.GetKey()] = append(out[m.GetKey()], string(m.GetValue()))
	}
	return out
}

// GRPCServer serves the scenarios with grpc-go.
type GRPCServer struct {
	difftestv1.UnimplementedDiffTestServer
}

func grpcSide(ctx context.Context) side {
	md, _ := grpcmetadata.FromIncomingContext(ctx)
	return side{
		incoming:   md,
		setHeader:  func(m map[string][]string) error { return grpc.SetHeader(ctx, m) },
		sendHeader: func(m map[string][]string) error { return grpc.SendHeader(ctx, m) },
		setTrailer: func(m map[string][]string) error { return grpc.SetTrailer(ctx, m) },
		status:     grpcStatus,
	}
}

func grpcStatus(code uint32, msg string, details []*difftestv1.Detail) error {
	st := grpcstatus.New(grpccodes.Code(code), msg)
	for _, d := range details {
		var err error
		if st, err = st.WithDetails(d); err != nil {
			return err
		}
	}
	return st.Err()
}

// Unary implements difftestv1.DiffTestServer.
func (GRPCServer) Unary(ctx context.Context, req *difftestv1.Request) (*difftestv1.Response, error) {
	return unary(ctx, grpcSide(ctx), req)
}

// Bidi implements difftestv1.DiffTestServer.
func (GRPCServer) Bidi(stream difftestv1.DiffTest_BidiServer) error {
	s := grpcSide(stream.Context())
	s.setHeader = func(m map[string][]string) error { return stream.SetHeader(m) }
	s.sendHeader = func(m map[string][]string) error { return stream.SendHeader(m) }
	s.setTrailer = func(m map[string][]string) error { stream.SetTrailer(m); return nil }
	return bidi(s, stream)
}

// ServerStream implements difftestv1.DiffTestServer.
func (GRPCServer) ServerStream(req *difftestv1.Request, stream difftestv1.DiffTest_ServerStreamServer) error {
	s := grpcSide(stream.Context())
	s.setHeader = func(m map[string][]string) error { return stream.SetHeader(m) }
	s.sendHeader = func(m map[string][]string) error { return stream.SendHeader(m) }
	s.setTrailer = func(m map[string][]string) error { stream.SetTrailer(m); return nil }
	return serverStreaming(s, req, stream)
}

// ClientStream implements difftestv1.DiffTestServer.
func (GRPCServer) ClientStream(stream difftestv1.DiffTest_ClientStreamServer) error {
	s := grpcSide(stream.Context())
	s.setHeader = func(m map[string][]string) error { return stream.SetHeader(m) }
	s.sendHeader = func(m map[string][]string) error { return stream.SendHeader(m) }
	s.setTrailer = func(m map[string][]string) error { stream.SetTrailer(m); return nil }
	return clientStreaming(s, stream)
}

// QUICServer serves the scenarios with quic-rpc-go.
type QUICServer struct {
	difftestv1quic.UnimplementedDiffTestServer
}

func quicSide(ctx context.Context) side {
	md, _ := metadata.FromIncomingContext(ctx)
	return side{
		incoming:   md,
		setHeader:  func(m map[string][]string) error { return rpc.SetHeader(ctx, m) },
		sendHeader: func(m map[string][]string) error { return rpc.SendHeader(ctx, m) },
		setTrailer: func(m map[string][]string) error { return rpc.SetTrailer(ctx, m) },
		status:     quicStatus,
	}
}

func quicStatus(code uint32, msg string, details []*difftestv1.Detail) error {
	st := status.New(codes.Code(code), msg)
	for _, d := range details {
		var err error
		if st, err = st.WithDetails(d); err != nil {
			return err
		}
	}
	return st.Err()
}

// Unary implements difftestv1quic.DiffTestServer.
func (QUICServer) Unary(ctx context.Context, req *difftestv1.Request) (*difftestv1.Response, error) {
	return unary(ctx, quicSide(ctx), req)
}

// Bidi implements difftestv1quic.DiffTestServer.
func (QUICServer) Bidi(stream difftestv1quic.DiffTest_BidiServer) error {
	s := quicSide(stream.Context())
	s.setHeader = func(m map[string][]string) error { return stream.SetHeader(m) }
	s.sendHeader = func(m map[string][]string) error { return stream.SendHeader(m) }
	s.setTrailer = func(m map[string][]string) error { stream.SetTrailer(m); return nil }
	return bidi(s, stream)
}

// ServerStream implements difftestv1quic.DiffTestServer.
func (QUICServer) ServerStream(req *difftestv1.Request, stream difftestv1quic.DiffTest_ServerStreamServer) error {
	s := quicSide(stream.Context())
	s.setHeader = func(m map[string][]string) error { return stream.SetHeader(m) }
	s.sendHeader = func(m map[string][]string) error { return stream.SendHeader(m) }
	s.setTrailer = func(m map[string][]string) error { stream.SetTrailer(m); return nil }
	return serverStreaming(s, req, stream)
}

// ClientStream implements difftestv1quic.DiffTestServer.
func (QUICServer) ClientStream(stream difftestv1quic.DiffTest_ClientStreamServer) error {
	s := quicSide(stream.Context())
	s.setHeader = func(m map[string][]string) error { return stream.SetHeader(m) }
	s.sendHeader = func(m map[string][]string) error { return stream.SendHeader(m) }
	s.setTrailer = func(m map[string][]string) error { stream.SetTrailer(m); return nil }
	return clientStreaming(s, stream)
}
