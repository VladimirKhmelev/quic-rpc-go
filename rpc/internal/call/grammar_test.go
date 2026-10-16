package call

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"pgregory.net/rapid"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame/framepb"
)

// The properties below check the readers against a reference model of the
// frame grammar in docs/protocol.md (TST-3): random frame sequences, valid
// and invalid, must be accepted or rejected exactly as the model says, with
// the same messages, header, status and trailer.

type tokenKind int

const (
	tokHeaders           tokenKind = iota // HEADERS with metadata only
	tokHeadersWithMethod                  // HEADERS a server must not send
	tokMessage
	tokTrailers
	tokBadTrailers // TRAILERS with a status code above Unauthenticated
	tokUnknownType // a frame type the protocol does not define
	tokTruncated   // a frame cut short by FIN; only ever last
)

type token struct {
	kind    tokenKind
	md      map[string][]string
	payload []byte
	code    codes.Code
	message string
}

type endKind int

const (
	endOK         endKind = iota // TRAILERS, then FIN
	endProtocol                  // a protocol violation
	endNoTrailers                // FIN without TRAILERS
	endEOF                       // server side: the client's FIN
)

type outcome struct {
	messages    [][]byte
	headerKnown bool
	header      map[string][]string
	end         endKind
	code        codes.Code
	message     string
	trailer     map[string][]string
}

var metadataGen = rapid.Custom(func(t *rapid.T) map[string][]string {
	md := map[string][]string{}
	for range rapid.IntRange(0, 3).Draw(t, "keys") {
		key := rapid.StringMatching(`[a-z0-9._-]{1,8}`).Draw(t, "key")
		binary := rapid.Bool().Draw(t, "binary")
		if binary {
			key += "-bin"
		}
		var values []string
		for range rapid.IntRange(1, 2).Draw(t, "values") {
			if binary {
				values = append(values, string(rapid.SliceOfN(rapid.Byte(), 0, 4).Draw(t, "binaryValue")))
			} else {
				values = append(values, rapid.StringMatching(`[ -~]{0,8}`).Draw(t, "value"))
			}
		}
		md[key] = values
	}
	return md
})

func drawToken(t *rapid.T, kind tokenKind) token {
	tk := token{kind: kind}
	switch kind {
	case tokHeaders:
		tk.md = metadataGen.Draw(t, "header")
	case tokMessage, tokTruncated:
		tk.payload = rapid.SliceOfN(rapid.Byte(), 1, 32).Draw(t, "payload")
	case tokTrailers:
		tk.code = codes.Code(rapid.IntRange(0, int(codes.Unauthenticated)).Draw(t, "code"))
		tk.message = rapid.StringMatching(`[ -~]{0,16}`).Draw(t, "message")
		tk.md = metadataGen.Draw(t, "trailer")
	}
	return tk
}

// drawResponse draws what a server sends: half the time a well-formed
// response, otherwise any sequence of frames.
func drawResponse(t *rapid.T) []token {
	var toks []token
	if rapid.Bool().Draw(t, "wellFormed") {
		if rapid.Bool().Draw(t, "withHeaders") {
			toks = append(toks, drawToken(t, tokHeaders))
		}
		for range rapid.IntRange(0, 3).Draw(t, "messages") {
			toks = append(toks, drawToken(t, tokMessage))
		}
		return append(toks, drawToken(t, tokTrailers))
	}
	kinds := []tokenKind{tokHeaders, tokHeadersWithMethod, tokMessage, tokTrailers, tokBadTrailers, tokUnknownType}
	for range rapid.IntRange(0, 6).Draw(t, "frames") {
		toks = append(toks, drawToken(t, rapid.SampledFrom(kinds).Draw(t, "kind")))
	}
	if rapid.Bool().Draw(t, "truncated") {
		toks = append(toks, drawToken(t, tokTruncated))
	}
	return toks
}

func encode(t *rapid.T, toks []token) []byte {
	var b []byte
	appendProto := func(typ frame.Type, m proto.Message) {
		var err error
		if b, err = frame.AppendProto(b, typ, m); err != nil {
			t.Fatalf("AppendProto: %v", err)
		}
	}
	for _, tk := range toks {
		switch tk.kind {
		case tokHeaders:
			appendProto(frame.TypeHeaders, &framepb.Headers{Metadata: frame.EncodeMetadata(tk.md)})
		case tokHeadersWithMethod:
			appendProto(frame.TypeHeaders, &framepb.Headers{Method: "/s/m"})
		case tokMessage:
			b = frame.AppendFrame(b, frame.TypeMessage, tk.payload)
		case tokTrailers:
			appendProto(frame.TypeTrailers, &framepb.Trailers{
				Code: uint32(tk.code), Message: tk.message, Metadata: frame.EncodeMetadata(tk.md),
			})
		case tokBadTrailers:
			appendProto(frame.TypeTrailers, &framepb.Trailers{Code: frame.MaxStatusCode + 1})
		case tokUnknownType:
			b = frame.AppendFrame(b, 0x07, nil)
		case tokTruncated:
			full := frame.AppendFrame(nil, frame.TypeMessage, tk.payload)
			b = append(b, full[:len(full)-1]...)
		}
	}
	return b
}

// modelClient is the reference for ClientRecv: [HEADERS] MESSAGE* TRAILERS FIN.
func modelClient(toks []token) outcome {
	var o outcome
	for i, tk := range toks {
		switch tk.kind {
		case tokHeaders:
			if o.headerKnown {
				o.end = endProtocol
				return o
			}
			o.headerKnown, o.header = true, tk.md
		case tokMessage:
			o.headerKnown = true
			o.messages = append(o.messages, tk.payload)
		case tokTrailers:
			o.headerKnown = true
			if i != len(toks)-1 {
				o.end = endProtocol // frames after TRAILERS
				return o
			}
			o.end, o.code, o.message, o.trailer = endOK, tk.code, tk.message, tk.md
			return o
		case tokBadTrailers:
			o.headerKnown = true
			o.end = endProtocol
			return o
		default: // HEADERS with a method, unknown type, truncated frame
			o.end = endProtocol
			return o
		}
	}
	o.end = endNoTrailers
	return o
}

func runClient(t *rapid.T, wire []byte, readHeaderFirst bool) outcome {
	headerCalls := 0
	r := NewClientRecv(frame.NewReader(bytes.NewReader(wire), frame.DefaultLimits()), func() { headerCalls++ })
	var got outcome
	var err error
	if readHeaderFirst {
		err = r.ReadHeader()
	}
	for err == nil {
		var p []byte
		if p, err = r.Next(); err == nil {
			got.messages = append(got.messages, p)
		}
	}
	got.headerKnown, got.header = r.HeaderKnown(), r.Header()
	switch {
	case errors.Is(err, io.EOF):
		got.end = endOK
		got.code, got.message, got.trailer = r.Status().Code(), r.Status().Message(), r.Trailer()
	case errors.Is(err, ErrProtocol):
		got.end = endProtocol
	case errors.Is(err, errNoTrailers):
		got.end = endNoTrailers
	default:
		t.Fatalf("unexpected error: %v", err)
	}
	if want := btoi(got.headerKnown); headerCalls != want {
		t.Fatalf("onHeader ran %d times, want %d", headerCalls, want)
	}
	return got
}

func TestClientRecvMatchesModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		toks := drawResponse(t)
		want := modelClient(toks)
		got := runClient(t, encode(t, toks), rapid.Bool().Draw(t, "readHeaderFirst"))
		compare(t, got, want)
	})
}

// modelServer is the reference for ServerRecv after the request HEADERS:
// MESSAGE* FIN.
func modelServer(toks []token) outcome {
	var o outcome
	for _, tk := range toks {
		if tk.kind != tokMessage {
			o.end = endProtocol
			return o
		}
		o.messages = append(o.messages, tk.payload)
	}
	o.end = endEOF
	return o
}

func TestServerRecvMatchesModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		toks := drawResponse(t)
		want := modelServer(toks)

		r := NewServerRecv(frame.NewReader(bytes.NewReader(encode(t, toks)), frame.DefaultLimits()))
		var got outcome
		var err error
		for err == nil {
			var p []byte
			if p, err = r.Next(); err == nil {
				got.messages = append(got.messages, p)
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			got.end = endEOF
		case errors.Is(err, ErrProtocol):
			got.end = endProtocol
		default:
			t.Fatalf("unexpected error: %v", err)
		}
		if r.Done() != (got.end == endEOF) {
			t.Fatalf("Done() = %v after ending with %v", r.Done(), got.end)
		}
		compare(t, got, want)
	})
}

func TestReadRequestHeadersMatchesModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		kind := rapid.IntRange(0, 4).Draw(t, "first")
		var (
			wire    []byte
			want    RequestHeaders
			wantErr error
		)
		switch kind {
		case 0: // a valid request
			want.Method = rapid.StringMatching(`/[a-z]{1,5}\.v1\.[A-Z][a-z]{0,5}/[A-Z][a-z]{0,5}`).Draw(t, "method")
			us := rapid.Uint64Range(0, frame.MaxTimeoutMicros).Draw(t, "timeoutUs")
			want.Timeout = time.Duration(us) * time.Microsecond //nolint:gosec // capped by MaxTimeoutMicros
			want.MD = metadataGen.Draw(t, "md")
			var err error
			if wire, err = AppendRequestHeaders(nil, want.Method, us, want.MD); err != nil {
				t.Fatalf("AppendRequestHeaders: %v", err)
			}
		case 1: // HEADERS without a method
			wire = encode(t, []token{drawToken(t, tokHeaders)})
			wantErr = ErrProtocol
		case 2: // the call starts with another frame
			k := rapid.SampledFrom([]tokenKind{tokMessage, tokTrailers, tokUnknownType, tokTruncated}).Draw(t, "kind")
			wire = encode(t, []token{drawToken(t, k)})
			wantErr = ErrProtocol
		case 3: // FIN right away
			wantErr = io.EOF
		case 4: // a valid request cut short
			full, err := AppendRequestHeaders(nil, "/s.v1.S/M", 0, nil)
			if err != nil {
				t.Fatalf("AppendRequestHeaders: %v", err)
			}
			wire = full[:rapid.IntRange(1, len(full)-1).Draw(t, "cut")]
			wantErr = ErrProtocol
		}

		got, err := ReadRequestHeaders(frame.NewReader(bytes.NewReader(wire), frame.DefaultLimits()))
		if wantErr != nil {
			if !errors.Is(err, wantErr) {
				t.Fatalf("got %v, want %v", err, wantErr)
			}
			return
		}
		if err != nil || got.Method != want.Method || got.Timeout != want.Timeout || !sameMD(got.MD, want.MD) {
			t.Fatalf("got %+v, %v; want %+v", got, err, want)
		}
	})
}

func compare(t *rapid.T, got, want outcome) {
	if got.end != want.end {
		t.Fatalf("ended with %v, want %v", got.end, want.end)
	}
	if len(got.messages) != len(want.messages) {
		t.Fatalf("%d messages, want %d", len(got.messages), len(want.messages))
	}
	for i := range got.messages {
		if !bytes.Equal(got.messages[i], want.messages[i]) {
			t.Fatalf("message %d = %x, want %x", i, got.messages[i], want.messages[i])
		}
	}
	if got.headerKnown != want.headerKnown || !sameMD(got.header, want.header) {
		t.Fatalf("header %v (known %v), want %v (known %v)", got.header, got.headerKnown, want.header, want.headerKnown)
	}
	if want.end == endOK && (got.code != want.code || got.message != want.message || !sameMD(got.trailer, want.trailer)) {
		t.Fatalf("status %v %q trailer %v, want %v %q trailer %v",
			got.code, got.message, got.trailer, want.code, want.message, want.trailer)
	}
}

// sameMD treats nil and empty metadata as equal.
func sameMD(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok || len(va) != len(vb) {
			return false
		}
		for i := range va {
			if va[i] != vb[i] {
				return false
			}
		}
	}
	return true
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (e endKind) String() string {
	return [...]string{"OK", "protocol violation", "no trailers", "EOF"}[e]
}
