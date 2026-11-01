package httpgrpc

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

// Server serves one HTTP version and closes active requests on Stop.
type Server struct {
	addr net.Addr
	stop func()
}

// Listen serves handler over TLS and exactly HTTP version 2 or 3.
func Listen(addr string, tlsConf *tls.Config, version int, handler http.Handler) (*Server, error) {
	if tlsConf == nil || (version != 2 && version != 3) {
		return nil, errors.New("httpgrpc: want TLS and HTTP version 2 or 3")
	}
	tc := tlsConf.Clone()
	checked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != version {
			http.Error(w, "wrong HTTP version", http.StatusHTTPVersionNotSupported)
			return
		}
		handler.ServeHTTP(w, r)
	})
	if version == 2 {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, err
		}
		tc.NextProtos = []string{"h2"}
		s := &http.Server{TLSConfig: tc, Handler: checked, ReadHeaderTimeout: 5 * time.Second}
		if err := http2.ConfigureServer(s, &http2.Server{MaxConcurrentStreams: 100}); err != nil {
			_ = l.Close()
			return nil, err
		}
		go func() { _ = s.Serve(tls.NewListener(l, tc)) }()
		return &Server{addr: l.Addr(), stop: func() { _ = s.Close() }}, nil
	}
	tc.NextProtos = []string{http3.NextProtoH3}
	// Listen (not ListenEarly) excludes early RPC execution. HTTP/3 needs
	// its own unidirectional control and QPACK streams.
	l, err := quic.ListenAddr(addr, tc, &quic.Config{MaxIncomingStreams: 100})
	if err != nil {
		return nil, err
	}
	s := &http3.Server{TLSConfig: tc, Handler: checked}
	go func() { _ = s.ServeListener(l) }()
	return &Server{addr: l.Addr(), stop: func() { _ = s.Close(); _ = l.Close() }}, nil
}

// Addr returns the server's listening address.
func (s *Server) Addr() net.Addr { return s.addr }

// Stop closes listeners, connections and request contexts.
func (s *Server) Stop() { s.stop() }
