// Package httpgrpc provides single-connection HTTP/2 and HTTP/3 transports
// for the connect-go gRPC baselines. It has no HTTP version fallback, proxy,
// application retries, compression or early application data.
package httpgrpc

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
)

// ErrClosed is returned after the client is closed.
var ErrClosed = errors.New("httpgrpc: client closed")

type connection struct {
	http.RoundTripper
	close  func() error
	usable func() bool
	dead   func() bool
}

// Client keeps one current connection to addr. Connections draining after
// GOAWAY keep their active calls; Close closes all of them. It is safe for
// concurrent use. A failed RPC is returned to the caller, never replayed.
type Client struct {
	addr    string
	version int
	tlsConf *tls.Config
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	current *connection
	all     map[*connection]struct{}
	dialing chan struct{}
}

// NewClient creates a lazy client for HTTP version 2 or 3. Config is cloned;
// the caller remains responsible for verifying the server certificate.
func NewClient(addr string, tlsConf *tls.Config, version int) (*Client, error) {
	if tlsConf == nil || (version != 2 && version != 3) {
		return nil, errors.New("httpgrpc: want TLS and HTTP version 2 or 3")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Client{addr: addr, tlsConf: tlsConf.Clone(), version: version, ctx: ctx, cancel: cancel, all: make(map[*connection]struct{})}, nil
}

// Connect waits for TLS and the peer's HTTP settings, without an extra probe
// RPC or PING that would add a network round trip to setup measurements.
func (c *Client) Connect(ctx context.Context) error { _, err := c.get(ctx); return err }

func (c *Client) get(ctx context.Context) (*connection, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if c.ctx.Err() != nil {
			c.mu.Unlock()
			return nil, ErrClosed
		}
		for old := range c.all {
			if old.dead() {
				delete(c.all, old)
			}
		}
		if conn := c.current; conn != nil && conn.usable() {
			c.mu.Unlock()
			return conn, nil
		}
		if pending := c.dialing; pending != nil {
			c.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-c.ctx.Done():
				return nil, ErrClosed
			}
		}
		done := make(chan struct{})
		c.dialing = done
		c.mu.Unlock()
		dialCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(c.ctx, cancel)
		conn, err := c.dial(dialCtx)
		stop()
		cancel()
		c.mu.Lock()
		closed := c.ctx.Err() != nil
		if err == nil && !closed {
			c.current = conn
			c.all[conn] = struct{}{}
		}
		c.dialing = nil
		close(done)
		c.mu.Unlock()
		if closed {
			if conn != nil {
				_ = conn.close()
			}
			return nil, ErrClosed
		}
		return conn, err
	}
}

// RoundTrip uses the current connection and checks the actual HTTP version.
func (c *Client) RoundTrip(req *http.Request) (*http.Response, error) {
	// Direct ClientConn APIs bypass http.Transport's request validation.
	// Reject malformed metadata before starting the streaming request body.
	for key, values := range req.Header {
		if !httpguts.ValidHeaderFieldName(key) {
			if req.Body != nil {
				_ = req.Body.Close()
			}
			return nil, connect.NewError(connect.CodeInternal, "invalid HTTP header name")
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				if req.Body != nil {
					_ = req.Body.Close()
				}
				return nil, connect.NewError(connect.CodeInternal, "invalid HTTP header value")
			}
		}
	}
	conn, err := c.get(req.Context())
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	resp, err := conn.RoundTrip(req)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		// A failed request is not replayed. On transport errors let a later
		// call dial again (also after HTTP/3 GOAWAY). Keep other active RPCs.
		if req.Context().Err() == nil {
			c.mu.Lock()
			if c.current == conn {
				c.current = nil
			}
			c.mu.Unlock()
		}
		return nil, err
	}
	if resp.ProtoMajor != c.version {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("httpgrpc: got %s, want HTTP/%d", resp.Proto, c.version)
	}
	return resp, nil
}

// IsClosed reports whether Close was called.
func (c *Client) IsClosed() bool { return c.ctx.Err() != nil }

// Close stops dialing and closes all current and draining connections.
func (c *Client) Close() error {
	c.mu.Lock()
	c.cancel()
	conns := c.all
	c.all = make(map[*connection]struct{})
	c.mu.Unlock()
	var errs []error
	for conn := range conns {
		errs = append(errs, conn.close())
	}
	return errors.Join(errs...)
}

func (c *Client) dial(ctx context.Context) (*connection, error) {
	tc := c.tlsConf.Clone()
	if c.version == 2 {
		tc.NextProtos = []string{"h2"}
		nc, err := (&tls.Dialer{NetDialer: &net.Dialer{}, Config: tc}).DialContext(ctx, "tcp", c.addr)
		if err != nil {
			return nil, err
		}
		tlsConn := nc.(*tls.Conn)
		if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
			_ = nc.Close()
			return nil, errors.New("httpgrpc: server did not negotiate h2")
		}
		observed := newSettingsConn(tlsConn)
		tr := &http2.Transport{DisableCompression: true, StrictMaxConcurrentStreams: true}
		cc, err := tr.NewClientConn(observed)
		if err != nil {
			_ = observed.Close()
			return nil, err
		}
		select {
		case <-observed.settings:
		case <-observed.done:
			_ = cc.Close()
			return nil, errors.New("httpgrpc: connection ended before HTTP/2 settings")
		case <-ctx.Done():
			_ = cc.Close()
			return nil, ctx.Err()
		}
		return &connection{RoundTripper: cc, close: cc.Close,
			usable: func() bool { s := cc.State(); return !s.Closed && !s.Closing }, dead: func() bool { return cc.State().Closed }}, nil
	}
	tc.NextProtos = []string{http3.NextProtoH3}
	// Same idle timeout and client keepalive policy as the native QUIC baseline.
	qc, err := quic.DialAddr(ctx, c.addr, tc, &quic.Config{MaxIncomingStreams: -1, KeepAlivePeriod: 15 * time.Second})
	if err != nil {
		return nil, err
	}
	cc := (&http3.Transport{DisableCompression: true}).NewClientConn(qc)
	select {
	case <-cc.ReceivedSettings():
	case <-qc.Context().Done():
		_ = qc.CloseWithError(0, "")
		return nil, context.Cause(qc.Context())
	case <-ctx.Done():
		_ = qc.CloseWithError(0, "")
		return nil, ctx.Err()
	}
	return &connection{RoundTripper: cc, close: func() error { return qc.CloseWithError(0, "client closed") },
		usable: func() bool { return qc.Context().Err() == nil }, dead: func() bool { return qc.Context().Err() != nil }}, nil
}
