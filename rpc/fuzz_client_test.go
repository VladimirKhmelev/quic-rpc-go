package rpc_test

import (
	"io"
	"testing"
	"testing/synctest"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/call"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
)

// FuzzClientResponse answers a call with arbitrary bytes. The client must
// not panic, must end the call with exactly one status, which RecvMsg keeps
// returning, and must not hang or leave goroutines behind.
func FuzzClientResponse(f *testing.F) {
	ok := mustFrames(f,
		func(b []byte) ([]byte, error) { return call.AppendResponseHeaders(b, map[string][]string{"k": {"v"}}) },
		func(b []byte) ([]byte, error) { return call.AppendMessage(b, wrapperspb.String("x")) },
		func(b []byte) ([]byte, error) { return call.AppendTrailers(b, nil, nil) },
	)
	failed := mustFrames(f, func(b []byte) ([]byte, error) {
		return call.AppendTrailers(b, status.New(codes.NotFound, "missing"), map[string][]string{"t": {"1"}})
	})
	for _, streaming := range []bool{false, true} {
		f.Add(ok, streaming)
		f.Add(failed, streaming)
		f.Add(ok[:len(ok)-1], streaming)
		f.Add([]byte{}, streaming)
	}

	f.Fuzz(func(t *testing.T, data []byte, streaming bool) {
		synctest.Test(t, func(t *testing.T) {
			l, d := transporttest.Mem(t, 10)
			// A fake server answers any call with data once the request ended.
			go func() {
				conn, err := l.Accept(t.Context())
				if err != nil {
					return
				}
				st, err := conn.AcceptStream(t.Context())
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, st)
				_, _ = st.Write(data)
				_ = st.Close()
			}()
			cc := rpc.NewClient(d)
			defer func() { _ = cc.Close() }()

			if !streaming {
				err := cc.Invoke(t.Context(), unaryMethod, wrapperspb.String("x"), new(msg))
				checkStatus(t, err)
				return
			}
			cs, err := cc.NewStream(t.Context(), &echoDesc.Streams[0], bidiMethod)
			if err != nil {
				t.Fatalf("NewStream: %v", err)
			}
			if err := cs.CloseSend(); err != nil {
				t.Fatalf("CloseSend: %v", err)
			}
			if _, err := cs.Header(); err != nil {
				checkStatus(t, err)
			}
			for {
				if err = cs.RecvMsg(new(msg)); err != nil {
					break
				}
			}
			if err != io.EOF { //nolint:errorlint // RecvMsg returns io.EOF itself
				checkStatus(t, err)
			}
			if again := cs.RecvMsg(new(msg)); again != err { //nolint:errorlint // the very same terminal error
				t.Fatalf("RecvMsg after the end returned %v, then %v", err, again)
			}
			_ = cs.Trailer()
		})
	})
}

// checkStatus fails unless err carries a status with a defined code.
func checkStatus(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	st, ok := status.FromError(err)
	if !ok || st.Code() > codes.Unauthenticated {
		t.Fatalf("the call ended with %v, which is not a valid status", err)
	}
}
