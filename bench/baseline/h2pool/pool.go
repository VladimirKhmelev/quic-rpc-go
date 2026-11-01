// Package h2pool distributes new RPCs round-robin across independent grpc-go
// HTTP/2 connections. Every connection uses the defaults of baseline/h2; no
// connection is reserved for a particular load or skipped based on its health.
package h2pool

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"google.golang.org/grpc"

	"github.com/VladimirKhmelev/quic-rpc-go/bench/baseline/h2"
)

// DefaultSize is the default number of TCP connections (GEN-8).
const DefaultSize = 4

// Pool is safe for concurrent use. Each RPC gets one connection; streaming
// messages stay on the connection chosen when the RPC started.
type Pool struct {
	conns     []*grpc.ClientConn
	next      atomic.Uint64
	closeOnce sync.Once
	closeErr  error
}

var _ grpc.ClientConnInterface = (*Pool)(nil)

// Dial creates size independent lazy clients. Connect can establish all of
// them before the benchmark schedule starts.
func Dial(addr string, tlsConf *tls.Config, size int, opts ...grpc.DialOption) (*Pool, error) {
	if size <= 0 {
		return nil, fmt.Errorf("h2pool: size %d: want a positive number", size)
	}
	p := &Pool{}
	for range size {
		cc, err := h2.Dial(addr, tlsConf, opts...)
		if err != nil {
			_ = p.Close()
			return nil, err
		}
		p.conns = append(p.conns, cc)
	}
	return p, nil
}

// Connect establishes all connections concurrently. Cancellation or failure
// stops this wait, without closing connections needed by other callers.
func (p *Pool) Connect(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var once sync.Once
	var first error
	for _, cc := range p.conns {
		wg.Go(func() {
			if err := h2.Connect(ctx, cc); err != nil {
				once.Do(func() { first = err; cancel() })
			}
		})
	}
	wg.Wait()
	return first
}

func (p *Pool) pick() *grpc.ClientConn {
	i := p.next.Add(1) - 1
	return p.conns[i%uint64(len(p.conns))]
}

// Invoke assigns one unary RPC to the next connection.
func (p *Pool) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	return p.pick().Invoke(ctx, method, args, reply, opts...)
}

// NewStream assigns a streaming RPC to the next connection for its lifetime.
func (p *Pool) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return p.pick().NewStream(ctx, desc, method, opts...)
}

// Close closes every connection and ends active RPCs. It is idempotent.
func (p *Pool) Close() error {
	p.closeOnce.Do(func() {
		var errs []error
		for _, cc := range p.conns {
			errs = append(errs, cc.Close())
		}
		p.closeErr = errors.Join(errs...)
	})
	return p.closeErr
}
