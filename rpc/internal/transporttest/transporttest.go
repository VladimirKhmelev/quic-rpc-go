// Package transporttest is a conformance suite for transport implementations
// plus helpers shared by transport and RPC tests. Running the same suite on
// the quic and mem transports keeps mem faithful to quic-go.
package transporttest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// Factory starts a listener whose connections allow at most maxStreams
// concurrent streams and returns a dialer for it. It must close both when
// the test ends.
type Factory func(t *testing.T, maxStreams int) (transport.Listener, transport.Dialer)

// blockedFor is how long a negative check waits to make sure an operation
// stays blocked. A correct transport never finishes it, so the wait cannot
// produce false failures.
const blockedFor = 100 * time.Millisecond

// Run runs the conformance suite.
func Run(t *testing.T, newTransport Factory) {
	tests := []struct {
		name string
		fn   func(*testing.T, Factory)
	}{
		{"HalfCloseEcho", testHalfCloseEcho},
		{"CloseCancelsSendContext", testCloseCancelsSendContext},
		{"Backpressure", testBackpressure},
		{"CancelWrite", testCancelWrite},
		{"CancelRead", testCancelRead},
		{"ConcurrentStreams", testConcurrentStreams},
		{"StreamLimit", testStreamLimit},
		{"ServerCannotOpenStreams", testServerCannotOpenStreams},
		{"AcceptStreamHonorsContext", testAcceptStreamHonorsContext},
		{"CloseWithError", testCloseWithError},
		{"ConnErrorCancelsContext", testConnErrorCancelsContext},
		{"ListenerClose", testListenerClose},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, newTransport) })
	}
}

// Context returns a context that ends with the test or after 10 seconds,
// so a broken transport fails the test instead of hanging it.
func Context(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// Pair dials a connection and accepts it on the server side.
func Pair(t *testing.T, newTransport Factory, maxStreams int) (client, server transport.Conn) {
	t.Helper()
	l, d := newTransport(t, maxStreams)
	ctx := Context(t)
	type result struct {
		c   transport.Conn
		err error
	}
	accepted := make(chan result, 1)
	go func() {
		c, err := l.Accept(ctx)
		accepted <- result{c, err}
	}()
	client, err := d.Dial(ctx)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	r := <-accepted
	if r.err != nil {
		t.Fatalf("Accept: %v", r.err)
	}
	t.Cleanup(func() {
		_ = client.CloseWithError(transport.NoError, "")
		_ = r.c.CloseWithError(transport.NoError, "")
	})
	return client, r.c
}

// Pattern returns n deterministic bytes that differ for different seeds.
func Pattern(n, seed int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*31 + seed*17) % 251)
	}
	return b
}

func open(t *testing.T, c transport.Conn) transport.Stream {
	t.Helper()
	s, err := c.OpenStream(Context(t))
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	return s
}

func accept(t *testing.T, c transport.Conn) transport.Stream {
	t.Helper()
	s, err := c.AcceptStream(Context(t))
	if err != nil {
		t.Fatalf("AcceptStream: %v", err)
	}
	return s
}

func write(t *testing.T, s transport.Stream, b []byte) {
	t.Helper()
	if _, err := s.Write(b); err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func closeStream(t *testing.T, s transport.Stream) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func readAll(t *testing.T, s transport.Stream) []byte {
	t.Helper()
	b, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return b
}

// withContext is a stream or a connection.
type withContext interface {
	Context() context.Context
}

// waitDone waits until the context of x is canceled and returns its cause.
func waitDone(t *testing.T, x withContext, what string) error {
	t.Helper()
	ctx := x.Context()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-Context(t).Done():
		t.Fatalf("%s context was not canceled", what)
		return nil
	}
}

func wantStreamError(t *testing.T, err error, code transport.ErrorCode, remote bool, what string) {
	t.Helper()
	se, ok := errors.AsType[*transport.StreamError](err)
	if !ok || se.Code != code || se.Remote != remote {
		t.Fatalf("%s: got %v, want %v", what, err, &transport.StreamError{Code: code, Remote: remote})
	}
}

func testHalfCloseEcho(t *testing.T, f Factory) {
	client, server := Pair(t, f, 10)

	cs := open(t, client)
	write(t, cs, []byte("ping"))
	closeStream(t, cs)

	ss := accept(t, server)
	if got := readAll(t, ss); string(got) != "ping" {
		t.Fatalf("server read %q, want %q", got, "ping")
	}
	// The client's FIN closed only its send direction: the server still writes.
	write(t, ss, []byte("pong"))
	closeStream(t, ss)
	if got := readAll(t, cs); string(got) != "pong" {
		t.Fatalf("client read %q, want %q", got, "pong")
	}
}

func testCloseCancelsSendContext(t *testing.T, f Factory) {
	client, server := Pair(t, f, 10)

	cs := open(t, client)
	if err := cs.Context().Err(); err != nil {
		t.Fatalf("context of a fresh stream: %v", err)
	}
	closeStream(t, cs)
	if cause := waitDone(t, cs, "stream"); !errors.Is(cause, transport.ErrStreamClosed) {
		t.Fatalf("cause after Close: %v, want ErrStreamClosed", cause)
	}

	ss := accept(t, server)
	if err := ss.Context().Err(); err != nil {
		t.Fatalf("the peer's send context was canceled by our Close: %v", err)
	}
	write(t, ss, []byte("still open"))
	closeStream(t, ss)
	if got := readAll(t, cs); string(got) != "still open" {
		t.Fatalf("client read %q", got)
	}
}

func testBackpressure(t *testing.T, f Factory) {
	client, server := Pair(t, f, 10)

	// Larger than quic-go's maximum stream window (6 MB) and mem's window.
	data := Pattern(10<<20, 1)
	cs := open(t, client)
	written := make(chan error, 1)
	go func() {
		_, err := cs.Write(data)
		if err == nil {
			err = cs.Close()
		}
		written <- err
	}()

	ss := accept(t, server)
	select {
	case err := <-written:
		t.Fatalf("Write of %d bytes finished while the peer read nothing (err: %v)", len(data), err)
	case <-time.After(blockedFor):
	}

	if got := readAll(t, ss); !bytes.Equal(got, data) {
		t.Fatalf("server read %d bytes, want the %d bytes written", len(got), len(data))
	}
	if err := <-written; err != nil {
		t.Fatalf("Write: %v", err)
	}
}

func testCancelWrite(t *testing.T, f Factory) {
	client, server := Pair(t, f, 10)

	cs := open(t, client)
	write(t, cs, []byte("partial"))
	cs.CancelWrite(transport.Cancelled)

	ss := accept(t, server)
	_, err := io.ReadAll(ss)
	wantStreamError(t, err, transport.Cancelled, true, "server Read after peer CancelWrite")

	_, err = cs.Write([]byte("x"))
	wantStreamError(t, err, transport.Cancelled, false, "client Write after own CancelWrite")
	cause := waitDone(t, cs, "client stream")
	wantStreamError(t, cause, transport.Cancelled, false, "client stream context cause")
}

func testCancelRead(t *testing.T, f Factory) {
	client, server := Pair(t, f, 10)

	cs := open(t, client)
	write(t, cs, []byte("x"))
	ss := accept(t, server)
	ss.CancelRead(transport.ProtocolError)

	_, err := ss.Read(make([]byte, 1))
	wantStreamError(t, err, transport.ProtocolError, false, "server Read after own CancelRead")

	// Writes succeed until the peer's STOP_SENDING arrives.
	ctx := Context(t)
	chunk := make([]byte, 1024)
	var werr error
	for werr == nil {
		if ctx.Err() != nil {
			t.Fatal("client writes never failed after the peer's CancelRead")
		}
		_, werr = cs.Write(chunk)
	}
	wantStreamError(t, werr, transport.ProtocolError, true, "client Write after peer CancelRead")
	cause := waitDone(t, cs, "client stream")
	wantStreamError(t, cause, transport.ProtocolError, true, "client stream context cause")
}

func testConcurrentStreams(t *testing.T, f Factory) {
	const streams = 100
	const size = 64 << 10
	client, server := Pair(t, f, streams)
	ctx := Context(t)

	errs := make(chan error, 2*streams)
	var wg sync.WaitGroup
	wg.Go(func() {
		for range streams {
			ss, err := server.AcceptStream(ctx)
			if err != nil {
				errs <- fmt.Errorf("AcceptStream: %w", err)
				return
			}
			wg.Go(func() {
				if _, err := io.Copy(ss, ss); err != nil {
					errs <- fmt.Errorf("server echo: %w", err)
					return
				}
				if err := ss.Close(); err != nil {
					errs <- fmt.Errorf("server Close: %w", err)
				}
			})
		}
	})
	for i := range streams {
		wg.Go(func() {
			errs <- echo(ctx, client, Pattern(size, i))
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// echo sends data and reads the echo concurrently. Reading only after the
// whole write would deadlock on QUIC once the echoes of all streams exceed
// the connection-level flow-control window.
func echo(ctx context.Context, c transport.Conn, data []byte) error {
	s, err := c.OpenStream(ctx)
	if err != nil {
		return fmt.Errorf("OpenStream: %w", err)
	}
	type result struct {
		b   []byte
		err error
	}
	read := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(s)
		read <- result{b, err}
	}()
	_, err = s.Write(data)
	if err == nil {
		err = s.Close()
	}
	if err != nil {
		s.CancelRead(transport.Cancelled)
		<-read
		return fmt.Errorf("send: %w", err)
	}
	r := <-read
	if r.err != nil {
		return fmt.Errorf("ReadAll: %w", r.err)
	}
	if !bytes.Equal(r.b, data) {
		return fmt.Errorf("echo returned %d bytes that differ from the %d sent", len(r.b), len(data))
	}
	return nil
}

func testStreamLimit(t *testing.T, f Factory) {
	client, server := Pair(t, f, 2)

	first := open(t, client)
	write(t, first, []byte("1"))
	second := open(t, client)
	write(t, second, []byte("2"))

	blocked, cancel := context.WithTimeout(Context(t), blockedFor)
	defer cancel()
	if _, err := client.OpenStream(blocked); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("OpenStream over the limit: %v, want context.DeadlineExceeded", err)
	}

	// Finish the first stream in both directions to free its slot.
	closeStream(t, first)
	ss := accept(t, server)
	if got := readAll(t, ss); string(got) != "1" {
		t.Fatalf("server read %q from the first stream", got)
	}
	closeStream(t, ss)
	readAll(t, first)

	third := open(t, client)
	write(t, third, []byte("3"))
}

func testServerCannotOpenStreams(t *testing.T, f Factory) {
	_, server := Pair(t, f, 10)
	ctx, cancel := context.WithTimeout(Context(t), blockedFor)
	defer cancel()
	if s, err := server.OpenStream(ctx); err == nil {
		t.Fatalf("server opened stream %v", s)
	}
}

func testAcceptStreamHonorsContext(t *testing.T, f Factory) {
	_, server := Pair(t, f, 10)
	ctx, cancel := context.WithCancel(Context(t))
	cancel()
	if _, err := server.AcceptStream(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("AcceptStream with a canceled context: %v, want context.Canceled", err)
	}
}

func testCloseWithError(t *testing.T, f Factory) {
	const (
		code   = transport.InternalError
		reason = "shutting down"
	)
	wantClosed := func(err error, remote bool, what string) {
		t.Helper()
		ce, ok := errors.AsType[*transport.ConnError](err)
		if !ok || ce.Err != nil || ce.Code != code || ce.Remote != remote || ce.Reason != reason {
			t.Fatalf("%s: got %v, want %v", what, err, &transport.ConnError{Code: code, Remote: remote, Reason: reason})
		}
	}

	client, server := Pair(t, f, 10)
	cs := open(t, client)
	write(t, cs, []byte("x"))
	ss := accept(t, server)

	if err := client.CloseWithError(code, reason); err != nil {
		t.Fatalf("CloseWithError: %v", err)
	}

	_, err := client.OpenStream(Context(t))
	wantClosed(err, false, "client OpenStream after own close")
	wantClosed(waitDone(t, client, "client connection"), false, "client connection context cause")

	wantClosed(waitDone(t, server, "server connection"), true, "server connection context cause")
	_, err = server.AcceptStream(Context(t))
	wantClosed(err, true, "server AcceptStream after peer close")
	_, err = io.ReadAll(ss)
	wantClosed(err, true, "server Read after peer close")
	wantClosed(waitDone(t, ss, "server stream"), true, "server stream context cause")
}

// testConnErrorCancelsContext checks that the connection context is done by
// the time anything reports the connection closed: rpc.ClientConn checks it
// to decide whether to dial again.
func testConnErrorCancelsContext(t *testing.T, f Factory) {
	closeConn := func(t *testing.T, c transport.Conn) {
		t.Helper()
		if err := c.CloseWithError(transport.NoError, "bye"); err != nil {
			t.Fatalf("CloseWithError: %v", err)
		}
	}
	wantDone := func(t *testing.T, c transport.Conn, err error, what string) {
		t.Helper()
		if _, ok := errors.AsType[*transport.ConnError](err); !ok {
			t.Fatalf("%s: got %v, want a *transport.ConnError", what, err)
		}
		if c.Context().Err() == nil {
			t.Fatalf("%s returned %v while the connection context is not done", what, err)
		}
	}

	t.Run("CloseWithError", func(t *testing.T) {
		client, _ := Pair(t, f, 10)
		closeConn(t, client)
		if client.Context().Err() == nil {
			t.Fatal("connection context is not done after CloseWithError returned")
		}
	})
	t.Run("OpenStream", func(t *testing.T) {
		client, server := Pair(t, f, 1)
		open(t, client)
		closeConn(t, server)
		// Blocks on the stream limit until the close reaches the client.
		_, err := client.OpenStream(Context(t))
		wantDone(t, client, err, "OpenStream")
	})
	t.Run("AcceptStream", func(t *testing.T) {
		client, server := Pair(t, f, 10)
		closeConn(t, client)
		_, err := server.AcceptStream(Context(t))
		wantDone(t, server, err, "AcceptStream")
	})
	t.Run("Read", func(t *testing.T) {
		client, server := Pair(t, f, 10)
		cs := open(t, client)
		write(t, cs, []byte("x"))
		accept(t, server)
		closeConn(t, server)
		_, err := io.ReadAll(cs)
		wantDone(t, client, err, "Read")
	})
}

func testListenerClose(t *testing.T, f Factory) {
	l, _ := f(t, 10)
	if l.Addr() == nil {
		t.Fatal("Addr is nil")
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := l.Accept(Context(t)); !errors.Is(err, transport.ErrListenerClosed) {
		t.Fatalf("Accept after Close: %v, want ErrListenerClosed", err)
	}
}
