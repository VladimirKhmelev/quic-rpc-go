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

// ErrServerStopped is returned by Serve after Stop or GracefulStop.
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
	conns     map[transport.Conn]*serverConn
	serving   bool
	stopped   bool
	// draining is set by GracefulStop: the listeners are closed and every
	// connection gets GOAWAY.
	draining bool
	// active counts the calls in flight. wake, if set, is closed when the
	// count drops to zero, a connection closes or the server stops: it
	// wakes GracefulStop.
	active int
	wake   chan struct{}
	// serveWG counts Serve loops, per-connection loops and GOAWAY senders,
	// not handlers.
	serveWG sync.WaitGroup
}

// serverConn is the state of a connection. Its fields are guarded by
// Server.mu.
type serverConn struct {
	// goingAway is set once the server sends GOAWAY on the connection; from
	// then on it refuses the streams it accepts.
	goingAway bool
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
		conns:     map[transport.Conn]*serverConn{},
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

// Serve accepts connections on l until Stop or GracefulStop is called or l
// fails; l is closed when Serve returns. It returns nil after Stop and
// GracefulStop, and ErrServerStopped if one of them came first.
func (s *Server) Serve(l transport.Listener) error {
	s.mu.Lock()
	if s.stopped || s.draining {
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
		sc, ok := s.addConn(conn)
		if !ok {
			_ = conn.CloseWithError(transport.NoError, "server stopped")
			return nil
		}
		go s.serveConn(conn, sc)
	}
}

// isStopped reports whether the server takes no more connections.
func (s *Server) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped || s.draining
}

// addConn registers conn and counts its loop in serveWG; it reports false
// once the server is stopped. A connection that comes in during
// GracefulStop gets GOAWAY at once.
func (s *Server) addConn(conn transport.Conn) (*serverConn, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, false
	}
	sc := &serverConn{}
	s.conns[conn] = sc
	s.serveWG.Add(1)
	if s.draining {
		s.goAwayLocked(conn, sc)
	}
	return sc, true
}

func (s *Server) serveConn(conn transport.Conn, sc *serverConn) {
	defer s.serveWG.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.wakeLocked()
		s.mu.Unlock()
	}()
	for {
		st, err := conn.AcceptStream(s.ctx)
		if err != nil {
			return
		}
		// Counted before the goroutine starts, so that GracefulStop never
		// misses an accepted call.
		if !s.callStarted(sc) {
			// The client opened the stream before GOAWAY reached it. The
			// call is not processed, so the client may send it again on
			// another connection.
			st.CancelRead(transport.Refused)
			st.CancelWrite(transport.Refused)
			continue
		}
		go func() {
			defer s.callDone()
			s.handleStream(st)
		}()
	}
}

// callStarted counts a call on sc in; it reports false if sc is going away.
func (s *Server) callStarted(sc *serverConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sc.goingAway {
		return false
	}
	s.active++
	return true
}

func (s *Server) callDone() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active--; s.active == 0 {
		s.wakeLocked()
	}
}

// wakeLocked wakes GracefulStop. s.mu must be held.
func (s *Server) wakeLocked() {
	if s.wake != nil {
		close(s.wake)
		s.wake = nil
	}
}

// goAwayLocked sends GOAWAY on conn unless it was sent already. s.mu must
// be held.
func (s *Server) goAwayLocked(conn transport.Conn, sc *serverConn) {
	if sc.goingAway {
		return
	}
	sc.goingAway = true
	s.serveWG.Go(func() {
		// Gives up once the connection closes or the server stops.
		st, err := conn.OpenUniStream(s.ctx)
		if err != nil {
			return
		}
		if _, err := st.Write(call.AppendGoAway(nil)); err == nil {
			_ = st.Close()
		}
	})
}

// GracefulStop stops the server gently. It closes the listeners at once and
// sends GOAWAY on every connection: the client opens no more calls on it and
// closes it once its calls are done. The calls the client opened before
// GOAWAY reached it are refused unprocessed, and the client sends them again
// on a new connection (docs/protocol.md). GracefulStop waits until the
// clients have closed all connections and the handlers have returned; then
// it stops as Stop does. If ctx ends first, or Stop is called meanwhile, it
// stops at once: the calls still in flight fail with Unavailable on the
// client.
func (s *Server) GracefulStop(ctx context.Context) {
	s.mu.Lock()
	s.draining = true
	listeners := make([]transport.Listener, 0, len(s.listeners))
	for l := range s.listeners {
		listeners = append(listeners, l)
	}
	for conn, sc := range s.conns {
		s.goAwayLocked(conn, sc)
	}
	s.mu.Unlock()
	for _, l := range listeners {
		_ = l.Close()
	}
	s.waitGone(ctx)
	s.Stop()
}

// waitGone waits until no connection is open and no call is in flight, or
// the server stops, or ctx ends.
func (s *Server) waitGone(ctx context.Context) {
	for {
		s.mu.Lock()
		if (len(s.conns) == 0 && s.active == 0) || s.stopped {
			s.mu.Unlock()
			return
		}
		if s.wake == nil {
			s.wake = make(chan struct{})
		}
		wake := s.wake
		s.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return
		}
	}
}

// Stop closes all listeners and connections at once and cancels the
// contexts of running handlers; calls in flight fail with Unavailable on the
// client. It waits for the accept loops, not for handlers to return, and
// ends a GracefulStop that is waiting.
func (s *Server) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		s.serveWG.Wait()
		return
	}
	s.stopped = true
	s.wakeLocked()
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
