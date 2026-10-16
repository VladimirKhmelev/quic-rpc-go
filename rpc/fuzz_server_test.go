package rpc_test

import (
	"io"
	"testing"
	"testing/synctest"

	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/call"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/transporttest"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// FuzzServerStream sends arbitrary bytes as a client stream. The server must
// not panic and must end the stream, with TRAILERS or a reset; the synctest
// bubble fails the input if anything hangs or a goroutine is left behind.
func FuzzServerStream(f *testing.F) {
	unary := mustFrames(f,
		func(b []byte) ([]byte, error) { return call.AppendRequestHeaders(b, unaryMethod, 0, nil) },
		func(b []byte) ([]byte, error) { return call.AppendMessage(b, wrapperspb.String("x")) },
	)
	bidi := mustFrames(f,
		func(b []byte) ([]byte, error) {
			return call.AppendRequestHeaders(b, bidiMethod, 1_000_000, map[string][]string{"k": {"v"}})
		},
		func(b []byte) ([]byte, error) { return call.AppendMessage(b, wrapperspb.String("a")) },
		func(b []byte) ([]byte, error) { return call.AppendMessage(b, wrapperspb.String("b")) },
	)
	f.Add(unary)
	f.Add(bidi)
	f.Add(unary[:len(unary)-1])
	f.Add([]byte{})
	f.Add([]byte{0x07, 0x00})

	f.Fuzz(func(t *testing.T, data []byte) {
		synctest.Test(t, func(t *testing.T) {
			l, d := transporttest.Mem(t, 10)
			srv := rpc.NewServer()
			srv.RegisterService(&echoDesc, &testServer{})
			served := make(chan error, 1)
			go func() { served <- srv.Serve(l) }()
			defer func() {
				srv.Stop()
				<-served
			}()

			conn, err := d.Dial(t.Context())
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			defer func() { _ = conn.CloseWithError(transport.NoError, "") }()
			st, err := conn.OpenStream(t.Context())
			if err != nil {
				t.Fatalf("OpenStream: %v", err)
			}
			// The server may reset the stream before reading everything.
			_, _ = st.Write(data)
			_ = st.Close()
			_, _ = io.Copy(io.Discard, st)
		})
	})
}

// mustFrames concatenates the frames built by the append functions.
func mustFrames(f *testing.F, appends ...func([]byte) ([]byte, error)) []byte {
	f.Helper()
	var b []byte
	for _, a := range appends {
		var err error
		if b, err = a(b); err != nil {
			f.Fatalf("build seed: %v", err)
		}
	}
	return b
}
