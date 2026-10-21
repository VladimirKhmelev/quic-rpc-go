package grpcbridge

import (
	"context"
	"sync"
	"time"

	grpcmetadata "google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	grpcstatus "google.golang.org/grpc/status"
)

// handlers are the stats handlers of a client or a server, called in the
// order they were given.
type handlers []stats.Handler

// tag calls TagRPC of every handler, each with the context the one before
// returned, as grpc-go does.
func (hs handlers) tag(ctx context.Context, method string) context.Context {
	for _, h := range hs {
		ctx = h.TagRPC(ctx, &stats.RPCTagInfo{FullMethodName: method})
	}
	return ctx
}

func (hs handlers) handle(ctx context.Context, s stats.RPCStats) {
	for _, h := range hs {
		h.HandleRPC(ctx, s)
	}
}

// call is the stats of one call.
type call struct {
	hs     handlers
	ctx    context.Context
	client bool
	begin  time.Time
	once   sync.Once
}

// begin tags a call and reports its start: Begin, then OutHeader with the
// outgoing metadata on the client or InHeader with the incoming metadata on
// the server. It returns nil without handlers.
func (hs handlers) begin(ctx context.Context, method string, client, clientStreams, serverStreams bool) *call {
	if len(hs) == 0 {
		return nil
	}
	c := &call{hs: hs, ctx: hs.tag(ctx, method), client: client, begin: time.Now()}
	hs.handle(c.ctx, &stats.Begin{Client: client, BeginTime: c.begin, IsClientStream: clientStreams, IsServerStream: serverStreams})
	if client {
		md, _ := grpcmetadata.FromOutgoingContext(c.ctx)
		hs.handle(c.ctx, &stats.OutHeader{Client: true, Header: md, FullMethod: method})
	} else {
		md, _ := grpcmetadata.FromIncomingContext(c.ctx)
		hs.handle(c.ctx, &stats.InHeader{Header: md, FullMethod: method})
	}
	return c
}

// context returns the context the handlers tagged, or ctx without them.
func (c *call) context(ctx context.Context) context.Context {
	if c == nil {
		return ctx
	}
	return c.ctx
}

// end reports the end of the call once, with err as the status of grpc-go
// the call ends with: errors without a status become one as grpc-go makes
// them, Canceled and DeadlineExceeded for context errors and Unknown
// otherwise. trailer is the trailer metadata a client received.
func (c *call) end(err error, trailer grpcmetadata.MD) {
	if c == nil {
		return
	}
	c.once.Do(func() {
		if _, ok := grpcstatus.FromError(err); !ok {
			err = grpcstatus.FromContextError(err).Err()
		}
		c.hs.handle(c.ctx, &stats.End{Client: c.client, BeginTime: c.begin, EndTime: time.Now(), Trailer: trailer, Error: err})
	})
}
