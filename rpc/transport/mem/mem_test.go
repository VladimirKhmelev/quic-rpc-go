package mem_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport/mem"
)

func factory(opts mem.Options) transporttest.Factory {
	return func(t *testing.T, maxStreams int) (transport.Listener, transport.Dialer) {
		o := opts
		if o.MaxIncomingStreams == 0 {
			o.MaxIncomingStreams = maxStreams
		}
		l := mem.Listen(o)
		t.Cleanup(func() { _ = l.Close() })
		return l, l
	}
}

func TestConformance(t *testing.T) {
	transporttest.Run(t, factory(mem.Options{}))
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

// finishes runs fn and fails the test if it blocks until the test context ends.
func finishes(t *testing.T, what string, fn func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-transporttest.Context(t).Done():
		t.Fatalf("%s blocked", what)
	}
}

func TestDefaults(t *testing.T) {
	defaults := func(t *testing.T, _ int) (transport.Listener, transport.Dialer) {
		l := mem.Listen(mem.Options{})
		t.Cleanup(func() { _ = l.Close() })
		return l, l
	}
	client, _ := transporttest.Pair(t, defaults, 0)
	ctx := transporttest.Context(t)

	s, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	finishes(t, "writing a full default window without a reader", func() error {
		_, err := s.Write(make([]byte, mem.DefaultStreamWindow))
		return err
	})
	for range mem.DefaultMaxIncomingStreams - 1 {
		if _, err := client.OpenStream(ctx); err != nil {
			t.Fatalf("OpenStream: %v", err)
		}
	}
	if got := mem.OpenStreams(client); got != mem.DefaultMaxIncomingStreams {
		t.Fatalf("open streams = %d, want %d", got, mem.DefaultMaxIncomingStreams)
	}
	if _, err := client.OpenStream(canceledContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenStream over the default limit: %v, want context.Canceled", err)
	}
}

func TestSlotHeldUntilAccepted(t *testing.T) {
	client, server := transporttest.Pair(t, factory(mem.Options{}), 1)
	ctx := transporttest.Context(t)

	s, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	s.CancelWrite(transport.Cancelled)
	s.CancelRead(transport.Cancelled)
	if got := mem.OpenStreams(client); got != 1 {
		t.Fatalf("open streams after reset, before accept = %d, want 1", got)
	}
	if _, err := client.OpenStream(canceledContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenStream while the slot is held: %v, want context.Canceled", err)
	}

	if _, err := server.AcceptStream(ctx); err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	if got := mem.OpenStreams(client); got != 0 {
		t.Fatalf("open streams after accept = %d, want 0", got)
	}
}

func TestSlotReleasedWhenBothDirectionsDone(t *testing.T) {
	client, server := transporttest.Pair(t, factory(mem.Options{}), 1)
	ctx := transporttest.Context(t)

	cs, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	ss, err := server.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	if _, err := io.ReadAll(ss); err != nil {
		t.Fatalf("server ReadAll: %v", err)
	}
	if got := mem.OpenStreams(client); got != 1 {
		t.Fatalf("open streams with the server direction still open = %d, want 1", got)
	}
	if err := ss.Close(); err != nil {
		t.Fatalf("server Close: %v", err)
	}
	if _, err := io.ReadAll(cs); err != nil {
		t.Fatalf("client ReadAll: %v", err)
	}
	if got := mem.OpenStreams(client); got != 0 {
		t.Fatalf("open streams after both directions finished = %d, want 0", got)
	}
}

func TestStreamSemantics(t *testing.T) {
	client, server := transporttest.Pair(t, factory(mem.Options{}), 10)
	ctx := transporttest.Context(t)

	cs, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	if n, err := cs.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) = %d, %v; want 0, nil", n, err)
	}
	if _, err := cs.Write([]byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, err := cs.Write([]byte("x")); err == nil {
		t.Fatal("Write after Close succeeded")
	}

	ss, err := server.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	if _, err := io.ReadAll(ss); err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	// After the peer read everything, a reset and a stop change nothing.
	cs.CancelWrite(transport.Cancelled)
	ss.CancelRead(transport.Cancelled)
	if _, err := ss.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("Read after EOF and late resets: %v, want io.EOF", err)
	}
	if cause := context.Cause(cs.Context()); !errors.Is(cause, transport.ErrStreamClosed) {
		t.Fatalf("client context cause: %v, want ErrStreamClosed", cause)
	}

	// Close after CancelWrite reports the reset.
	reset, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	reset.CancelWrite(transport.InternalError)
	err = reset.Close()
	if se, ok := errors.AsType[*transport.StreamError](err); !ok || se.Code != transport.InternalError {
		t.Fatalf("Close after CancelWrite: %v, want a StreamError with INTERNAL_ERROR", err)
	}
}

func TestConnCloseUnblocksWriter(t *testing.T) {
	client, _ := transporttest.Pair(t, factory(mem.Options{StreamWindow: 16}), 1)
	ctx := transporttest.Context(t)

	s, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	blocked := make(chan error, 1)
	go func() {
		_, err := s.Write(make([]byte, 32))
		blocked <- err
	}()
	if err := client.CloseWithError(transport.NoError, "bye"); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}
	err = <-blocked
	if ce, ok := errors.AsType[*transport.ConnError](err); !ok || ce.Remote {
		t.Fatalf("blocked Write after own close: %v, want a local ConnError", err)
	}
}

func TestClientAcceptStreamWaits(t *testing.T) {
	client, _ := transporttest.Pair(t, factory(mem.Options{}), 1)
	if _, err := client.AcceptStream(canceledContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("client AcceptStream: %v, want context.Canceled", err)
	}
}

func TestListener(t *testing.T) {
	ctx := transporttest.Context(t)
	l := mem.Listen(mem.Options{})
	other := mem.Listen(mem.Options{})
	if l.Addr().Network() != "mem" || l.Addr().String() == other.Addr().String() {
		t.Fatalf("addresses %v and %v: want distinct mem addresses", l.Addr(), other.Addr())
	}
	if _, err := l.Accept(canceledContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Accept with a canceled context: %v", err)
	}

	queued, err := l.Dial(ctx)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = queued.CloseWithError(transport.NoError, "") })
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := l.Dial(ctx); !errors.Is(err, transport.ErrListenerClosed) {
		t.Fatalf("Dial after Close: %v, want ErrListenerClosed", err)
	}
	if _, err := l.Accept(ctx); err != nil {
		t.Fatalf("Accept of a connection queued before Close: %v", err)
	}
	if _, err := l.Accept(ctx); !errors.Is(err, transport.ErrListenerClosed) {
		t.Fatalf("second Accept after Close: %v, want ErrListenerClosed", err)
	}
}

func TestDialWaitsForAcceptQueue(t *testing.T) {
	l := mem.Listen(mem.Options{})
	ctx := transporttest.Context(t)
	for range mem.AcceptQueueSize {
		c, err := l.Dial(ctx)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		t.Cleanup(func() { _ = c.CloseWithError(transport.NoError, "") })
	}
	if _, err := l.Dial(canceledContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Dial with a full accept queue: %v, want context.Canceled", err)
	}
}
