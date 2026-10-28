package mem

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/VladimirKhmelev/quic-rpc-go/rpc/transport"
)

var errWriteAfterClose = errors.New("mem: write on closed stream")

// streamPair is the state shared by the two ends of a stream. The stream
// holds its slot until both directions are done and the server accepted it,
// as in quic-go, which counts a stream until the application accepted it.
type streamPair struct {
	pending atomic.Int32
	release func()
}

func (p *streamPair) step() {
	if p.pending.Add(-1) == 0 {
		p.release()
	}
}

// stream is one end of a stream: it reads from in and writes to out.
type stream struct {
	in   *pipe
	out  *pipe
	pair *streamPair
}

func newStreamPair(s *session, release func()) (client, server *stream) {
	pair := &streamPair{release: release}
	pair.pending.Store(3) // two directions and the accept
	c2s := newPipe(s, clientSide, s.window, pair.step)
	s2c := newPipe(s, serverSide, s.window, pair.step)
	return &stream{in: s2c, out: c2s, pair: pair}, &stream{in: c2s, out: s2c, pair: pair}
}

// newUniStreamPair returns the ends of a stream from the server to the
// client: the server end only writes, the client end only reads.
func newUniStreamPair(s *session, release func()) (server, client *stream) {
	pair := &streamPair{release: release}
	pair.pending.Store(2) // one direction and the accept
	s2c := newPipe(s, serverSide, s.window, pair.step)
	return &stream{out: s2c, pair: pair}, &stream{in: s2c, pair: pair}
}

func (s *stream) Read(b []byte) (int, error)           { return s.in.read(b) }
func (s *stream) Write(b []byte) (int, error)          { return s.out.write(b) }
func (s *stream) Close() error                         { return s.out.closeWrite() }
func (s *stream) CancelWrite(code transport.ErrorCode) { s.out.cancelWrite(code) }
func (s *stream) CancelRead(code transport.ErrorCode)  { s.in.cancelRead(code) }
func (s *stream) Context() context.Context             { return s.out.ctx }

// pipe is one direction of a stream. One goroutine reads and one writes;
// Close and the Cancel methods may be called from any goroutine.
type pipe struct {
	s          *session
	writerSide int
	window     int
	onDone     func()

	// ctx belongs to the writing end: it is canceled when this direction
	// can no longer be written to.
	ctx       context.Context
	cancelCtx context.CancelCauseFunc

	// readable and writable wake up a blocked reader or writer. They have
	// capacity 1, so a wake-up sent before the wait is not lost.
	readable chan struct{}
	writable chan struct{}

	mu       sync.Mutex
	buf      bytes.Buffer
	fin      bool  // the writer called Close
	eofRead  bool  // the reader got io.EOF
	readErr  error // returned by read after a reset
	writeErr error // returned by write after a reset
	done     bool
}

func newPipe(s *session, writerSide, window int, onDone func()) *pipe {
	ctx, cancel := context.WithCancelCause(s.ends[writerSide].ctx)
	return &pipe{
		s:          s,
		writerSide: writerSide,
		window:     window,
		onDone:     onDone,
		ctx:        ctx,
		cancelCtx:  cancel,
		readable:   make(chan struct{}, 1),
		writable:   make(chan struct{}, 1),
	}
}

func (p *pipe) write(b []byte) (int, error) {
	n := 0
	for {
		p.mu.Lock()
		if err := p.writeBlockedLocked(); err != nil {
			p.mu.Unlock()
			return n, err
		}
		if k := min(p.window-p.buf.Len(), len(b)-n); k > 0 {
			p.buf.Write(b[n : n+k])
			n += k
			notify(p.readable)
		}
		p.mu.Unlock()
		if n == len(b) {
			return n, nil
		}
		select {
		case <-p.writable:
		case <-p.s.done:
		}
	}
}

func (p *pipe) writeBlockedLocked() error {
	switch {
	case isClosed(p.s.done):
		return p.s.errFor(p.writerSide)
	case p.writeErr != nil:
		return p.writeErr
	case p.fin:
		return errWriteAfterClose
	default:
		return nil
	}
}

func (p *pipe) read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	for {
		p.mu.Lock()
		switch {
		case isClosed(p.s.done):
			p.mu.Unlock()
			return 0, p.s.errFor(1 - p.writerSide)
		case p.readErr != nil:
			p.mu.Unlock()
			return 0, p.readErr
		case p.buf.Len() > 0:
			n, _ := p.buf.Read(b)
			notify(p.writable)
			p.mu.Unlock()
			return n, nil
		case p.fin:
			p.eofRead = true
			p.checkDoneLocked()
			p.mu.Unlock()
			return 0, io.EOF
		}
		p.mu.Unlock()
		select {
		case <-p.readable:
		case <-p.s.done:
		}
	}
}

func (p *pipe) closeWrite() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.writeBlockedLocked(); err != nil {
		if errors.Is(err, errWriteAfterClose) {
			return nil
		}
		return err
	}
	p.fin = true
	notify(p.readable)
	p.cancelCtx(transport.ErrStreamClosed)
	return nil
}

func (p *pipe) cancelWrite(code transport.ErrorCode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.writeErr != nil {
		return
	}
	p.writeErr = &transport.StreamError{Code: code}
	if p.readErr == nil && !p.eofRead {
		p.readErr = &transport.StreamError{Code: code, Remote: true}
		p.buf.Reset()
	}
	p.cancelCtx(p.writeErr)
	p.wakeLocked()
}

func (p *pipe) cancelRead(code transport.ErrorCode) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.readErr != nil || p.eofRead {
		return
	}
	p.readErr = &transport.StreamError{Code: code}
	p.buf.Reset()
	// The peer's QUIC stack answers STOP_SENDING with a reset of its send
	// direction, carrying the same code.
	if p.writeErr == nil {
		p.writeErr = &transport.StreamError{Code: code, Remote: true}
		p.cancelCtx(p.writeErr)
	}
	p.wakeLocked()
}

func (p *pipe) wakeLocked() {
	notify(p.readable)
	notify(p.writable)
	p.checkDoneLocked()
}

// checkDoneLocked reports the direction as done once all data was read up to
// FIN or the direction was reset.
func (p *pipe) checkDoneLocked() {
	if p.done {
		return
	}
	if (p.fin && p.eofRead) || p.readErr != nil || p.writeErr != nil {
		p.done = true
		p.onDone()
	}
}

func notify(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
