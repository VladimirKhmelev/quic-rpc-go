package rpcquic

import (
	"crypto/tls"

	"github.com/quic-go/quic-go"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// StateForTest inspects the actual QUIC handshake, without extending the API.
func StateForTest(c transport.Conn) quic.ConnectionState { return c.(*conn).qc.ConnectionState() }

// CacheForTest inspects the dialer's automatically allocated cache.
func CacheForTest(d *Dialer) tls.ClientSessionCache { return d.cache }
