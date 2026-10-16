package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/codes"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/call"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/internal/frame"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/metadata"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/status"
	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

// ErrServerStopped is returned by Serve after Stop.
var ErrServerStopped = errors.New("rpc: the server has been stopped")

// Server serves registered services. Each call runs its handler in its own
// goroutine. Handler panics are not recovered, as in grpc-go.
type Server struct {
	// ctx is the parent of every handler's context; Stop cancels it.
	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	services  map[string]*serviceInfo
	listeners map[transport.Listener]struct{}
	conns     map[transport.Conn]struct{}
	serving   bool
	stopped   bool
	// serveWG counts Serve loops and per-connection loops, not handlers.
	serveWG sync.WaitGroup
}

var _ ServiceRegistrar = (*Server)(nil)

type serviceInfo struct {
	impl    any
	methods map[string]*MethodDesc
	streams map[string]*StreamDesc
}

// NewServer returns a server with no services.
func NewServer() *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{
		ctx:       ctx,
		cancel:    cancel,
		services:  map[string]*serviceInfo{},
		listeners: map[transport.Listener]struct{}{},
		conns:     map[transport.Conn]struct{}{},
	}
}

// RegisterService registers impl as the implementation of desc. It panics if
// impl does not implement desc.HandlerType, if the service is registered
// twice or if Serve was already called.
func (s *Server) RegisterService(desc *ServiceDesc, impl any) {
	if impl != nil && desc.HandlerType != nil {
		want := reflect.TypeOf(desc.HandlerType).Elem()
		if got := reflect.TypeOf(impl); !got.Implements(want) {
			panic(fmt.Sprintf("rpc: RegisterService: handler of type %v does not satisfy %v", got, want))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serving {
		panic(fmt.Sprintf("rpc: RegisterService(%s) called after Serve", desc.ServiceName))
	}
	if _, dup := s.services[desc.ServiceName]; dup {
		panic(fmt.Sprintf("rpc: RegisterService found a duplicate service %s", desc.ServiceName))
	}
	info := &serviceInfo{
		impl:    impl,
		methods: make(map[string]*MethodDesc, len(desc.Methods)),
		streams: make(map[string]*StreamDesc, len(desc.Streams)),
	}
	for i := range desc.Methods {
		info.methods[desc.Methods[i].MethodName] = &desc.Methods[i]
	}
	for i := range desc.Streams {
		info.streams[desc.Streams[i].StreamName] = &desc.Streams[i]
	}
	s.services[desc.ServiceName] = info
}

// Serve accepts connections on l until Stop is called or l fails; l is
// closed when Serve returns. It returns nil after Stop.
func (s *Server) Serve(l transport.Listener) error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		_ = l.Close()
		return ErrServerStopped
	}
	s.serving = true
	s.listeners[l] = struct{}{}
	s.serveWG.Add(1)
	s.mu.Unlock()
	defer s.serveWG.Done()
	defer func() {
		s.mu.Lock()
		delete(s.listeners, l)
		s.mu.Unlock()
		_ = l.Close()
	}()

	for {
		conn, err := l.Accept(s.ctx)
		if err != nil {
			if s.isStopped() {
				return nil
			}
			return err
		}
		if !s.addConn(conn) {
			_ = conn.CloseWithError(transport.NoError, "server stopped")
			return nil
		}
		go s.serveConn(conn)
	}
}

func (s *Server) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// addConn registers conn and counts its loop in serveWG; it reports false
// once the server is stopped.
func (s *Server) addConn(conn transport.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	s.conns[conn] = struct{}{}
	s.serveWG.Add(1)
	return true
}

func (s *Server) serveConn(conn transport.Conn) {
	defer s.serveWG.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
	}()
	for {
		st, err := conn.AcceptStream(s.ctx)
		if err != nil {
			return
		}
		go s.handleStream(st)
	}
}

// Stop closes all listeners and connections at once and cancels the
// contexts of running handlers; calls in flight fail with Unavailable on the
// client. It waits for the accept loops, not for handlers to return.
func (s *Server) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		s.serveWG.Wait()
		return
	}
	s.stopped = true
	listeners := make([]transport.Listener, 0, len(s.listeners))
	for l := range s.listeners {
		listeners = append(listeners, l)
	}
	conns := make([]transport.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	for _, l := range listeners {
		_ = l.Close()
	}
	// Close the connections before canceling the handlers: a canceled
	// handler resets its stream, and the client must see the closed
	// connection (Unavailable), not that reset (Canceled).
	for _, c := range conns {
		_ = c.CloseWithError(transport.NoError, "server stopped")
	}
	s.cancel()
	s.serveWG.Wait()
}

func (s *Server) handleStream(st transport.Stream) {
	fr := frame.NewReader(st, frame.DefaultLimits())
	req, err := call.ReadRequestHeaders(fr)
	if err != nil {
		code := call.ResetCode(err)
		st.CancelWrite(code)
		st.CancelRead(code)
		return
	}

	var (
		ctx    context.Context
		cancel context.CancelFunc
	)
	if req.Timeout > 0 {
		ctx, cancel = context.WithTimeout(s.ctx, req.Timeout)
	} else {
		ctx, cancel = context.WithCancel(s.ctx)
	}
	defer cancel()
	// The client cancels a call by stopping to read; that ends the send
	// direction of our stream and its context.
	stopWatch := context.AfterFunc(st.Context(), cancel)
	defer stopWatch()

	ss := &serverStream{st: st, recv: call.NewServerRecv(fr)}
	ctx = metadata.NewIncomingContext(ctx, metadata.MD(req.MD))
	ctx = context.WithValue(ctx, serverStreamKey{}, ss)
	ss.ctx = ctx
	// A deadline, Stop or the client's cancellation resets the stream,
	// unless the call already finished.
	ss.stopAbort = context.AfterFunc(ctx, func() { ss.abort(transport.Cancelled) })

	service, method, _ := strings.Cut(strings.TrimPrefix(req.Method, "/"), "/")
	info := s.service(service)
	switch {
	case info == nil:
		ss.finish(status.Newf(codes.Unimplemented, "unknown service %v", service))
	case info.methods[method] != nil:
		s.processUnary(ctx, ss, info, info.methods[method])
	case info.streams[method] != nil:
		ss.desc = info.streams[method]
		err := ss.desc.Handler(info.impl, ss)
		ss.finish(handlerStatus(err))
	default:
		ss.finish(status.Newf(codes.Unimplemented, "unknown method %v for service %v", method, service))
	}
}

func (s *Server) service(name string) *serviceInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.services[name]
}

// processUnary reads the whole request before calling the handler, as
// grpc-go does, and sends the response, TRAILERS and FIN in one write.
func (s *Server) processUnary(ctx context.Context, ss *serverStream, info *serviceInfo, md *MethodDesc) {
	payload, err := ss.recv.Next()
	if errors.Is(err, io.EOF) {
		ss.finish(status.New(codes.Internal,
			"cardinality violation: received no request message from non-client-streaming RPC"))
		return
	}
	if err != nil {
		ss.finish(ss.recvFailed(err))
		return
	}
	if _, err := ss.recv.Next(); !errors.Is(err, io.EOF) {
		if err == nil {
			ss.finish(status.New(codes.Internal,
				"cardinality violation: received multiple request messages for non-client-streaming RPC"))
		} else {
			ss.finish(ss.recvFailed(err))
		}
		return
	}

	dec := func(m any) error {
		if err := call.Unmarshal(payload, m); err != nil {
			return status.Errorf(codes.Internal, "unmarshal request: %v", err)
		}
		return nil
	}
	resp, err := md.Handler(info.impl, ctx, dec, nil)
	if err != nil {
		ss.finish(handlerStatus(err))
		return
	}
	ss.finishUnary(resp)
}

// handlerStatus converts a handler's error to the status sent to the
// client, as grpc-go does: status errors keep their status, context errors
// become Canceled or DeadlineExceeded, anything else Unknown.
func handlerStatus(err error) *status.Status {
	if err == nil {
		return nil
	}
	if st, ok := status.FromError(err); ok {
		return st
	}
	return status.FromContextError(err)
}
