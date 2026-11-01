package main

import (
	"crypto/tls"
	"encoding/json"
	"io"
	"sync"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/loadgen"
)

// event is a line of JSON that load prints on its standard output for run
// to follow: a phase of the load generator, or the TLS of a connection
// unlike those before it.
type event struct {
	Phase loadgen.Phase `json:"phase,omitempty"`
	TLS   *tlsInfo      `json:"tls,omitempty"`
}

// tlsInfo is what a connection negotiated (GEN-7).
type tlsInfo struct {
	Version     string `json:"version"`
	CipherSuite string `json:"cipher_suite"`
	Group       string `json:"group"`
	ALPN        string `json:"alpn,omitempty"`
}

// events prints events, each TLS setup once.
type events struct {
	mu   sync.Mutex
	enc  *json.Encoder
	seen map[tlsInfo]bool
}

func newEvents(w io.Writer) *events {
	return &events{enc: json.NewEncoder(w), seen: map[tlsInfo]bool{}}
}

func (e *events) phase(p loadgen.Phase) { e.print(event{Phase: p}) }

// verify is a tls.Config.VerifyConnection that prints the TLS setup.
func (e *events) verify(cs tls.ConnectionState) error {
	info := tlsInfo{Version: tls.VersionName(cs.Version), CipherSuite: tls.CipherSuiteName(cs.CipherSuite), Group: cs.CurveID.String(), ALPN: cs.NegotiatedProtocol}
	e.mu.Lock()
	seen := e.seen[info]
	e.seen[info] = true
	e.mu.Unlock()
	if !seen {
		e.print(event{TLS: &info})
	}
	return nil
}

func (e *events) print(ev event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.enc.Encode(ev) // nobody may be listening
}
