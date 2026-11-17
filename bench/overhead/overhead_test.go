// Package overhead measures RPC stacks over memory transports, separately
// from network experiments. Neither stack uses TLS, sockets or compression.
package overhead

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/baseline/h2"
	benchv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1"
	"github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1/benchv1quic"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/mem"
)

const bufferBytes = 1 << 20

var (
	backends = []string{"direct", "protobuf", "rpc-mem", "grpc-bufconn"}
	kinds    = []string{"unary", "bidi"}
	sizes    = []int{0, 100, 1024, 65536}
	// Keep each response observable, including the controls' allocations.
	responseSink proto.Message
)

func unaryReply(req *benchv1.CallRequest) (*benchv1.CallResponse, error) {
	if int64(req.GetResponseSize()) != int64(len(req.GetPayload())) {
		return nil, errors.New("overhead: request and response payload sizes must match")
	}
	return &benchv1.CallResponse{Payload: req.GetPayload()}, nil
}

func echoReply(req *benchv1.EchoMessage) *benchv1.EchoMessage {
	return &benchv1.EchoMessage{Seq: req.Seq, Payload: req.Payload}
}

type echoStream interface {
	Send(*benchv1.EchoMessage) error
	Recv() (*benchv1.EchoMessage, error)
	CloseSend() error
}

type endpoint struct {
	unary func(context.Context, *benchv1.CallRequest) (*benchv1.CallResponse, error)
	echo  func(context.Context) (echoStream, error)
}

type grpcService struct {
	benchv1.UnimplementedBenchServer
}

func (grpcService) Call(_ context.Context, req *benchv1.CallRequest) (*benchv1.CallResponse, error) {
	return unaryReply(req)
}

func (grpcService) Echo(stream grpc.BidiStreamingServer[benchv1.EchoMessage, benchv1.EchoMessage]) error {
	return serveEcho(stream)
}

type rpcService struct {
	benchv1quic.UnimplementedBenchServer
}

func (rpcService) Call(_ context.Context, req *benchv1.CallRequest) (*benchv1.CallResponse, error) {
	return unaryReply(req)
}

func (rpcService) Echo(stream rpc.BidiStreamingServer[benchv1.EchoMessage, benchv1.EchoMessage]) error {
	return serveEcho(stream)
}

func serveEcho(stream interface {
	Send(*benchv1.EchoMessage) error
	Recv() (*benchv1.EchoMessage, error)
}) error {
	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(echoReply(req)); err != nil {
			return err
		}
	}
}

func newEndpoint(t testing.TB, backend string) endpoint {
	t.Helper()
	if backend == "rpc-mem" {
		listener := mem.Listen(mem.Options{})
		server := rpc.NewServer()
		benchv1quic.RegisterBenchServer(server, rpcService{})
		served := make(chan error, 1)
		go func() { served <- server.Serve(listener) }()
		cc := rpc.NewClient(listener)
		t.Cleanup(func() {
			_ = cc.Close()
			server.Stop()
			if err := <-served; err != nil && !errors.Is(err, rpc.ErrServerStopped) {
				t.Errorf("Serve: %v", err)
			}
		})
		c := benchv1quic.NewBenchClient(cc)
		return endpoint{
			unary: func(ctx context.Context, req *benchv1.CallRequest) (*benchv1.CallResponse, error) {
				return c.Call(ctx, req)
			},
			echo: func(ctx context.Context) (echoStream, error) { return c.Echo(ctx) },
		}
	}
	if backend != "grpc-bufconn" {
		t.Fatalf("unknown RPC backend %q", backend)
	}
	listener := bufconn.Listen(bufferBytes)
	server := grpc.NewServer()
	benchv1.RegisterBenchServer(server, grpcService{})
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	cc, err := grpc.NewClient("passthrough:///memory",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithNoProxy(),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		server.Stop()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cc.Close()
		server.Stop()
		_ = listener.Close()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	if err := h2.Connect(t.Context(), cc); err != nil {
		t.Fatal(err)
	}
	c := benchv1.NewBenchClient(cc)
	return endpoint{
		unary: func(ctx context.Context, req *benchv1.CallRequest) (*benchv1.CallResponse, error) {
			return c.Call(ctx, req)
		},
		echo: func(ctx context.Context) (echoStream, error) { return c.Echo(ctx) },
	}
}

type operation struct {
	step   func() (proto.Message, error)
	finish func() error
	want   proto.Message
}

func newOperation(t testing.TB, backend, kind string, size int) operation {
	t.Helper()
	if size < 0 || size > 65536 {
		t.Fatal("unsupported payload size")
	}
	payload := bytes.Repeat([]byte{0x5a}, size)
	request := &benchv1.CallRequest{Payload: payload, ResponseSize: uint32(size)} //nolint:gosec // validated 0..65536
	message := &benchv1.EchoMessage{Seq: 1, Payload: payload}
	op := operation{finish: func() error { return nil }}
	switch kind {
	case "unary":
		op.want = &benchv1.CallResponse{Payload: payload}
	case "bidi":
		op.want = message
	default:
		t.Fatal("unsupported RPC kind")
	}
	if backend == "direct" || backend == "protobuf" {
		op.step = func() (proto.Message, error) {
			var in proto.Message = request
			if kind == "bidi" {
				in = message
			}
			if backend == "protobuf" {
				var err error
				in, err = codecCopy(in)
				if err != nil {
					return nil, err
				}
			}
			var out proto.Message
			if kind == "unary" {
				response, err := unaryReply(in.(*benchv1.CallRequest)) //nolint:forcetypeassert // selected by kind
				if err != nil {
					return nil, err
				}
				out = response
			} else {
				out = echoReply(in.(*benchv1.EchoMessage)) //nolint:forcetypeassert // selected by kind
			}
			if backend == "protobuf" {
				return codecCopy(out)
			}
			return out, nil
		}
		return op
	}
	e := newEndpoint(t, backend)
	if kind == "unary" {
		op.step = func() (proto.Message, error) { return e.unary(t.Context(), request) }
		return op
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	stream, err := e.echo(ctx)
	if err != nil {
		t.Fatal(err)
	}
	op.step = func() (proto.Message, error) {
		if err := stream.Send(message); err != nil {
			return nil, err
		}
		return stream.Recv()
	}
	op.finish = func() error {
		defer cancel()
		if err := stream.CloseSend(); err != nil {
			return err
		}
		_, err := stream.Recv()
		if err == nil {
			return errors.New("stream: unexpected extra response")
		}
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("stream final status: %w", err)
		}
		return nil
	}
	return op
}

func codecCopy(in proto.Message) (proto.Message, error) {
	data, err := proto.Marshal(in)
	if err != nil {
		return nil, err
	}
	out := in.ProtoReflect().Type().New().Interface()
	return out, proto.Unmarshal(data, out)
}

func BenchmarkRPCOverhead(b *testing.B) {
	for _, backend := range backends {
		b.Run("Backend="+backend, func(b *testing.B) {
			for _, kind := range kinds {
				b.Run("Kind="+kind, func(b *testing.B) {
					for _, size := range sizes {
						b.Run(fmt.Sprintf("Bytes=%d", size), func(b *testing.B) {
							op := newOperation(b, backend, kind, size)
							warm, err := op.step()
							if err != nil || !proto.Equal(warm, op.want) {
								b.Fatalf("warm-up: %v, %v", warm, err)
							}
							b.ReportAllocs()
							for b.Loop() {
								out, err := op.step()
								if err != nil {
									b.Fatal(err)
								}
								if kind == "unary" {
									if len(out.(*benchv1.CallResponse).Payload) != size { //nolint:forcetypeassert // selected by kind
										b.Fatal("incorrect payload size")
									}
								} else {
									m := out.(*benchv1.EchoMessage) //nolint:forcetypeassert // selected by kind
									if m.Seq != 1 || len(m.Payload) != size {
										b.Fatal("incorrect echoed message")
									}
								}
								responseSink = out
							}
							if err := op.finish(); err != nil {
								b.Fatal(err)
							}
						})
					}
				})
			}
		})
	}
}

func TestRoundTrips(t *testing.T) {
	for _, backend := range backends {
		for _, kind := range kinds {
			for _, size := range sizes {
				t.Run(fmt.Sprintf("%s/%s/%d", backend, kind, size), func(t *testing.T) {
					op := newOperation(t, backend, kind, size)
					for range 3 {
						out, err := op.step()
						if err != nil || !proto.Equal(out, op.want) {
							t.Fatalf("round trip: %v, %v", out, err)
						}
					}
					if err := op.finish(); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
