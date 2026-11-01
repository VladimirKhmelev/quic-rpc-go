package httpgrpc

import (
	"crypto/tls"
	"sync"
)

// settingsConn observes the first HTTP/2 frame without consuming or changing
// bytes. x/net/http2 exposes connection state but no settings-ready channel.
// Observing arrival avoids polling or a probe adding a whole RTT to Connect.
// Only the HTTP/2 reader accesses header, count and remaining.
type settingsConn struct {
	*tls.Conn
	settings  chan struct{}
	done      chan struct{}
	once      sync.Once
	header    [9]byte
	count     int
	remaining int
	valid     bool
	finished  bool
}

func newSettingsConn(c *tls.Conn) *settingsConn {
	return &settingsConn{Conn: c, settings: make(chan struct{}), done: make(chan struct{})}
}

func (c *settingsConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if !c.finished {
		c.observe(p[:n])
	}
	if err != nil {
		c.once.Do(func() { close(c.done) })
	}
	return n, err
}

func (c *settingsConn) observe(p []byte) {
	if c.count < len(c.header) {
		n := copy(c.header[c.count:], p)
		c.count += n
		p = p[n:]
		if c.count < len(c.header) {
			return
		}
		h := c.header
		c.remaining = int(h[0])<<16 | int(h[1])<<8 | int(h[2])
		c.valid = h[3] == 4 && h[4]&1 == 0 && h[5]&0x7f == 0 && h[6] == 0 && h[7] == 0 && h[8] == 0 && c.remaining%6 == 0
	}
	c.remaining -= min(len(p), c.remaining)
	if c.remaining == 0 {
		c.finished = true
		if c.valid {
			close(c.settings)
		}
	}
}

func (c *settingsConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}
