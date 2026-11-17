package common

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	benchv1 "github.com/VladimirKhmelev/quic-rpc-go/internal/gen/bench/v1"
)

// BulkStream receives one bounded payload at a time, without retaining it.
type BulkStream interface {
	Recv() (seq uint64, size int, err error)
}

// BulkResult counts all received payload bytes and chunks, including those
// received before a failure. Success also requires EOF with an OK status.
type BulkResult struct {
	Bytes  int
	Chunks int
}

func validateBulk(total, chunk, maxPayload int) error {
	if total < 0 || total > math.MaxInt32 || chunk <= 0 || chunk > maxPayload {
		return &StatusError{Code: 3, Message: fmt.Sprintf("bulk: total must be 0..%d and chunk 1..%d", math.MaxInt32, maxPayload)}
	}
	return nil
}

func bulkRequest(total, chunk, maxPayload int) (*benchv1.BulkRequest, error) {
	if err := validateBulk(total, chunk, maxPayload); err != nil {
		return nil, err
	}
	return &benchv1.BulkRequest{TotalBytes: uint64(total), ChunkBytes: uint32(chunk)}, nil //nolint:gosec // validated in 0..MaxInt32
}

// sendBulk reuses the service's immutable payload storage. A synchronous Send
// serializes each chunk before the next one; no buffer grows with total_bytes.
func (r responses) sendBulk(ctx context.Context, req *benchv1.BulkRequest, send func(*benchv1.BulkChunk) error) error {
	if req.TotalBytes > math.MaxInt32 || req.ChunkBytes > math.MaxInt32 {
		return &StatusError{Code: 3, Message: "bulk: size exceeds MaxInt32"}
	}
	if err := validateBulk(int(req.TotalBytes), int(req.ChunkBytes), len(r.zeros)); err != nil {
		return err
	}
	for seq, offset := uint64(0), uint64(0); offset < req.TotalBytes; seq++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(uint64(req.ChunkBytes), req.TotalBytes-offset)
		if err := send(&benchv1.BulkChunk{Seq: seq, Payload: r.zeros[:n]}); err != nil {
			return err
		}
		offset += n
	}
	return nil
}

type bulkReceiver struct {
	ctx     context.Context
	receive func() (*benchv1.BulkChunk, error)
	result  func(error) error
}

func (s bulkReceiver) Recv() (uint64, int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, 0, bulkContextError(err)
	}
	m, err := s.receive()
	if ctxErr := s.ctx.Err(); ctxErr != nil {
		return 0, 0, bulkContextError(ctxErr)
	}
	if err != nil {
		return 0, 0, s.result(err)
	}
	return m.Seq, len(m.Payload), nil
}

func bulkContextError(err error) error {
	code := uint32(1)
	if errors.Is(err, context.DeadlineExceeded) {
		code = 4
	}
	return &StatusError{Code: code, Message: err.Error()}
}

// ReceiveBulk validates sequence numbers, every chunk's exact size, total
// bytes and the terminal status. Cancellation also releases a malformed peer's
// stream. The result retains partial progress on failure.
func ReceiveBulk(ctx context.Context, c Client, total, chunk int) (BulkResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s, err := c.Bulk(ctx, total, chunk)
	if err != nil {
		return BulkResult{}, err
	}
	return receiveBulk(s, total, chunk)
}

func receiveBulk(s BulkStream, total, chunk int) (BulkResult, error) {
	var result BulkResult
	for {
		seq, n, err := s.Recv()
		if errors.Is(err, io.EOF) {
			if result.Bytes == total {
				return result, nil
			}
			return result, &StatusError{Code: 15, Message: "bulk: premature EOF"}
		}
		if err != nil {
			return result, err
		}
		if n < 0 || n > math.MaxInt-result.Bytes {
			return result, &StatusError{Code: 15, Message: "bulk: invalid byte count"}
		}
		wantSeq, wantSize := uint64(result.Chunks), min(chunk, total-result.Bytes) //nolint:gosec // nonnegative chunk counter
		result.Bytes += n
		result.Chunks++
		if seq != wantSeq || n <= 0 || n != wantSize {
			return result, &StatusError{Code: 15, Message: "bulk: unexpected sequence or chunk size"}
		}
	}
}
