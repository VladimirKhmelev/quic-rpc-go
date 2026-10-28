package call

import (
	"errors"
	"io"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
)

// AppendGoAway appends what the server sends on its unidirectional stream
// when it goes away: one GOAWAY frame. The server then closes the stream.
func AppendGoAway(b []byte) []byte {
	return frame.AppendFrame(b, frame.TypeGoAway, nil)
}

// ReadGoAway reads the server's unidirectional stream on the client side:
// GOAWAY, then FIN.
func ReadGoAway(fr *frame.Reader) error {
	f, err := fr.ReadFrame()
	if errors.Is(err, io.EOF) {
		return protocolError("the unidirectional stream ended without GOAWAY")
	}
	if err != nil {
		return readError(err)
	}
	if f.Type != frame.TypeGoAway {
		return protocolError("the unidirectional stream carries %v, want GOAWAY", f.Type)
	}
	if _, err := fr.ReadFrame(); !errors.Is(err, io.EOF) {
		if err == nil {
			return protocolError("frames after GOAWAY")
		}
		return readError(err)
	}
	return nil
}
