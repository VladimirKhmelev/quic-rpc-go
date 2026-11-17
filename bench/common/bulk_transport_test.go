package common_test

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/common"
)

type observedBulkClient struct {
	common.Client
	observe func()
}

func (c observedBulkClient) Bulk(ctx context.Context, total, chunk int) (common.BulkStream, error) {
	s, err := c.Client.Bulk(ctx, total, chunk)
	return observedBulkStream{s, c.observe}, err
}

type observedBulkStream struct {
	common.BulkStream
	observe func()
}

func (s observedBulkStream) Recv() (uint64, int, error) {
	seq, n, err := s.BulkStream.Recv()
	if err == nil && seq%400 == 0 {
		s.observe()
	}
	return seq, n, err
}

func TestBulkTransports(t *testing.T) {
	forEach(t, func(t *testing.T, e *env) {
		for _, size := range []int{0, 1, 65535, 65536, 65539} {
			r, err := common.ReceiveBulk(ctx(t), e.client, size, 65536)
			if err != nil || r.Bytes != size || r.Chunks != (size+65535)/65536 {
				t.Fatalf("%d bytes: %+v, %v", size, r, err)
			}
		}
		// Retained memory is bounded by stream/connection windows and a
		// chunk, rather than the full 100 MiB. Check live heap after GC.
		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)
		client := observedBulkClient{e.client, func() {
			runtime.GC()
			var now runtime.MemStats
			runtime.ReadMemStats(&now)
			if now.HeapAlloc > before.HeapAlloc+32<<20 {
				t.Fatalf("bulk retained %d extra heap bytes", now.HeapAlloc-before.HeapAlloc)
			}
		}}
		r, err := common.ReceiveBulk(ctx(t), client, 100<<20, 64<<10)
		if err != nil || r.Bytes != 100<<20 || r.Chunks != 1600 {
			t.Fatalf("100 MiB: %+v, %v", r, err)
		}
		for _, sizes := range [][2]int{{-1, 1}, {1, 0}, {1, common.MaxResponse + 1}} {
			if _, err := e.client.Bulk(ctx(t), sizes[0], sizes[1]); common.Code(err) != 3 {
				t.Fatalf("invalid sizes %v: %v", sizes, err)
			}
		}
	})
}

func TestBulkInterrupted(t *testing.T) {
	for _, reason := range []string{"cancel", "deadline", "stop"} {
		t.Run(reason, func(t *testing.T) {
			forEach(t, func(t *testing.T, e *env) {
				if err := e.client.Connect(ctx(t)); err != nil {
					t.Fatal(err)
				}
				callCtx, cancel := context.WithCancel(ctx(t))
				if reason == "deadline" {
					cancel()
					callCtx, cancel = context.WithTimeout(ctx(t), 200*time.Millisecond)
				}
				defer cancel()
				s, err := e.client.Bulk(callCtx, 100<<20, 64<<10)
				if err != nil {
					t.Fatal(err)
				}
				if _, n, err := s.Recv(); err != nil || n != 64<<10 {
					t.Fatalf("first chunk: %d, %v", n, err)
				}
				want := uint32(1)
				switch reason {
				case "cancel":
					cancel()
				case "deadline":
					<-callCtx.Done()
					want = 4
				case "stop":
					e.server.Stop()
					want = 14
				}
				for {
					_, _, err := s.Recv()
					if err != nil {
						// connect-go cannot distinguish an abruptly truncated HTTP
						// body from a malformed peer envelope. Preserve that diagnosis.
						ce, _ := errors.AsType[*common.StatusError](err)
						truncatedHTTP := reason == "stop" && (e.transport == common.ConnectH2 || e.transport == common.GRPCH3) && ce != nil && ce.Code == 13 && ce.Message == "protocol error: incomplete envelope: unexpected EOF"
						if common.Code(err) != want && !truncatedHTTP {
							t.Fatalf("after %s: %v", reason, err)
						}
						break
					}
					if reason != "stop" {
						t.Fatal("buffered chunk delivered after cancellation")
					}
				}
				if reason != "stop" {
					if _, err := e.client.Call(ctx(t), nil, 1); err != nil {
						t.Fatalf("call after interrupted stream: %v", err)
					}
				}
			})
		})
	}
}
