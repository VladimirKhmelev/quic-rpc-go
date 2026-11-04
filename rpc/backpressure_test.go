package rpc_test

import (
	"errors"
	"io"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
)

func TestSlowReaderBoundsMemory(t *testing.T) {
	for _, tr := range transporttest.Transports {
		t.Run(tr.Name, func(t *testing.T) {
			if tr.Name == "mem" {
				synctest.Test(t, func(t *testing.T) { slowReaderBoundsMemory(t, tr.Factory) })
			} else {
				slowReaderBoundsMemory(t, tr.Factory)
			}
		})
	}
}

func slowReaderBoundsMemory(t *testing.T, f transporttest.Factory) {
	t.Helper()
	const (
		chunks = 1024
		size   = 64 << 10
		// A single stream can grow to 6 MiB in quic-go. Include both
		// endpoints, framing and packet buffers, but not the whole 64 MiB.
		budget = 32 << 20
	)
	payload := strings.Repeat("bounded", size/7) + strings.Repeat("x", size%7)
	chunk := wrapperspb.String(payload)
	var produced atomic.Int32
	e := newEnv(t, f, &testServer{serverStream: func(_ *msg, stream rpc.ServerStreamingServer[msg]) error {
		for range chunks {
			if err := stream.Send(chunk); err != nil {
				return err
			}
			produced.Add(1)
		}
		return nil
	}})
	ctx := transporttest.Context(t)
	if _, err := e.unary(ctx, "warm"); err != nil {
		t.Fatal(err)
	}
	liveHeap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	base := liveHeap()
	checkMemory := func(received int) {
		t.Helper()
		if heap := liveHeap(); heap > base+budget {
			t.Fatalf("after %d chunks: retained heap grew by %d bytes, budget %d", received, heap-base, budget)
		}
	}
	stream, err := e.cc.NewStream(ctx, &echoDesc.Streams[1], serverStreamMethod)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.SendMsg(wrapperspb.String("start")); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseSend()
	// Keep the reader stopped long enough to exhaust the transport window.
	// For mem this is virtual time; QUIC needs its real network goroutines.
	time.Sleep(100 * time.Millisecond)
	if produced.Load() == chunks {
		t.Fatal("sender buffered the entire response while the client read nothing")
	}
	checkMemory(0)
	for i := range chunks {
		out := new(msg)
		if err := stream.RecvMsg(out); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		if out.Value != payload {
			t.Fatalf("chunk %d was corrupted", i)
		}
		if (i+1)%8 == 0 {
			time.Sleep(2 * time.Millisecond)
		}
		if (i+1)%256 == 0 {
			checkMemory(i + 1)
		}
	}
	if err := stream.RecvMsg(new(msg)); !errors.Is(err, io.EOF) {
		t.Fatalf("final status: %v, want EOF", err)
	}
	if produced.Load() != chunks {
		t.Fatalf("produced %d chunks, want %d", produced.Load(), chunks)
	}
	checkMemory(chunks)
}
