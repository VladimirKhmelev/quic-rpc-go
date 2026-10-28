package call

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// errReader returns its data, then err.
type errReader struct {
	data []byte
	err  error
}

func (r *errReader) Read(b []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(b, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestReadGoAway(t *testing.T) {
	goAway := AppendGoAway(nil)
	reset := &transport.StreamError{Code: transport.Cancelled, Remote: true}
	for _, tt := range []struct {
		name string
		r    io.Reader
		want error // nil, ErrProtocol or the reader's error
	}{
		{"GOAWAY", bytes.NewReader(goAway), nil},
		{"nothing", bytes.NewReader(nil), ErrProtocol},
		{"another frame", bytes.NewReader(frame.AppendFrame(nil, frame.TypeMessage, nil)), ErrProtocol},
		{"payload", bytes.NewReader(frame.AppendFrame(nil, frame.TypeGoAway, []byte{0})), ErrProtocol},
		{"two frames", bytes.NewReader(append(AppendGoAway(nil), goAway...)), ErrProtocol},
		{"cut after GOAWAY", bytes.NewReader(append(AppendGoAway(nil), byte(frame.TypeGoAway))), ErrProtocol},
		{"reset before GOAWAY", &errReader{err: reset}, reset},
		{"reset after GOAWAY", &errReader{data: goAway, err: reset}, reset},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := ReadGoAway(frame.NewReader(tt.r, frame.DefaultLimits()))
			if tt.want == nil && err != nil || !errors.Is(err, tt.want) {
				t.Fatalf("ReadGoAway: %v, want %v", err, tt.want)
			}
		})
	}
}
