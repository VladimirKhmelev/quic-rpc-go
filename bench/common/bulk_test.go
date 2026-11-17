package common

import (
	"context"
	"crypto/tls"
	"io"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/quic-go/quic-go/http3"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/baseline/httpgrpc"
	benchv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1"
)

type bulkFrame struct {
	seq uint64
	n   int
}

type scriptedBulk struct {
	frames []bulkFrame
	err    error
}

func (s *scriptedBulk) Recv() (uint64, int, error) {
	if len(s.frames) == 0 {
		return 0, 0, s.err
	}
	f := s.frames[0]
	s.frames = s.frames[1:]
	return f.seq, f.n, nil
}

func TestBulkValidation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		frames []bulkFrame
		err    error
		code   uint32
		bytes  int
	}{
		{"valid", []bulkFrame{{0, 4}, {1, 1}}, io.EOF, 0, 5},
		{"short", []bulkFrame{{0, 4}}, io.EOF, 15, 4},
		{"sequence", []bulkFrame{{1, 4}}, io.EOF, 15, 4},
		{"short-chunk", []bulkFrame{{0, 3}}, io.EOF, 15, 3},
		{"empty-chunk", []bulkFrame{{0, 0}}, io.EOF, 15, 0},
		{"extra", []bulkFrame{{0, 4}, {1, 1}, {2, 1}}, io.EOF, 15, 6},
		{"error-after-all-bytes", []bulkFrame{{0, 4}, {1, 1}}, &StatusError{Code: 13}, 13, 5},
		{"error-midway", []bulkFrame{{0, 4}}, &StatusError{Code: 14}, 14, 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, err := receiveBulk(&scriptedBulk{frames: tt.frames, err: tt.err}, 5, 4)
			if Code(err) != tt.code || r.Bytes != tt.bytes {
				t.Fatalf("%+v, %v", r, err)
			}
		})
	}
}

func TestBulkSenderReusesStorage(t *testing.T) {
	l, _ := (MessageLimits{}).Resolve()
	s := newResponses(l)
	const total, chunk = 100 << 20, 64 << 10
	n, count := 0, uint64(0)
	err := s.sendBulk(t.Context(), &benchv1.BulkRequest{TotalBytes: total, ChunkBytes: chunk}, func(m *benchv1.BulkChunk) error {
		if m.Seq != count || len(m.Payload) != chunk || &m.Payload[0] != &s.zeros[0] {
			t.Fatal("unexpected sequence, payload size or copied backing storage")
		}
		n += len(m.Payload)
		count++
		return nil
	})
	if err != nil || n != total || count != 1600 {
		t.Fatalf("%d bytes, %d chunks: %v", n, count, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.sendBulk(ctx, &benchv1.BulkRequest{TotalBytes: total, ChunkBytes: chunk}, func(*benchv1.BulkChunk) error { t.Fatal("sent after cancellation"); return nil }); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestBulkHTTP3ShutdownStatus(t *testing.T) {
	httpClient, err := httpgrpc.NewClient("localhost:1", &tls.Config{MinVersion: tls.VersionTLS13}, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = httpClient.Close() }()
	c := &connectClient{http: httpClient}
	for _, tt := range []struct {
		cause error
		want  uint32
	}{
		{&http3.Error{Remote: true, ErrorCode: http3.ErrCodeNoError}, 14},
		{&http3.Error{Remote: true, ErrorCode: http3.ErrCodeMessageError}, 13},
		{io.ErrUnexpectedEOF, 13},
	} {
		err := connect.NewError(connect.CodeInternal, "incomplete envelope").WithCause(tt.cause)
		if got := c.result(t.Context(), err); Code(got) != tt.want {
			t.Fatalf("cause=%v got=%v", tt.cause, got)
		}
	}
}
