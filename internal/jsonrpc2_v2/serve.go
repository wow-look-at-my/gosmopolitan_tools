// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package jsonrpc2

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// Listener is implemented by protocols to accept new inbound connections.
type Listener interface {
	// Accept accepts an inbound connection to a server.
	Accept(context.Context) (io.ReadWriteCloser, error)

	// Close closes the listener. Any blocked Accept or Dial operations will unblock and return errors.
	Close() error

	// Dialer returns a dialer that can be used to connect to this listener locally.
	Dialer() Dialer
}

// Dialer is used by clients to dial a server.
type Dialer interface {
	// Dial returns a new communication byte stream to a listening server.
	Dial(ctx context.Context) (io.ReadWriteCloser, error)
}

// Server is a running server that is accepting incoming connections.
type Server struct {
	listener Listener
	binder   Binder
	async    *async

	shutdownOnce sync.Once
	closing      atomic.Int32 // atomic: set to nonzero when Shutdown is called
}

// Dial uses the dialer to make a new connection, wraps the returned
// reader and writer using the framer to make a stream. And then builds
// a connection on top of that stream using the binder.
func Dial(ctx context.Context, dialer Dialer, binder Binder, onDone func()) (*Connection, error) {
	// dial a server
	rwc, err := dialer.Dial(ctx)
	if err != nil {
		return nil, err
	}
	return bindConnection(ctx, rwc, binder, onDone), nil
}

// NewServer starts a new server listening for incoming connections and
// returns it. This returns a fully running and connected server, it does not
// block on the listener.
func NewServer(ctx context.Context, listener Listener, binder Binder) *Server {
	server := &Server{
		listener: listener,
		binder:   binder,
		async:    newAsync(),
	}
	go server.run(ctx)
	return server
}

// Wait returns only when the server has shut down.
func (s *Server) Wait() error {
	return s.async.wait()
}

// Shutdown informs the server to stop accepting new connections.
func (s *Server) Shutdown() {
	s.shutdownOnce.Do(func() {
		s.closing.Store(1)
		s.listener.Close()
	})
}

// run accepts incoming connections from the listener, If IdleTimeout is
// non-zero, run exits after there are no clients. For this duration,
// otherwise it exits only on error.
func (s *Server) run(ctx context.Context) {
	defer s.async.done()

	var activeConns sync.WaitGroup
	for {
		rwc, err := s.listener.Accept(ctx)
		if err != nil {
			// Only Shutdown closes the listener.
			if s.closing.Load() == 0 {
				s.async.setError(err)
			}
			// We are done generating new connections for good.
			break
		}

		// A new inbound connection.
		activeConns.Add(1)
		_ = bindConnection(ctx, rwc, s.binder, activeConns.Done) // unregisters itself when done
	}
	activeConns.Wait()
}

// NewIdleListener wraps a listener with an idle timeout.
//
// When there are no active connections for at least the timeout duration,
// calls to Accept will fail with ErrIdleTimeout.
//
// A connection is considered inactive as soon as its Close method is called.
func NewIdleListener(timeout time.Duration, wrap Listener) Listener {
	l := &idleListener{
		wrapped:   wrap,
		timeout:   timeout,
		active:    make(chan int, 1),
		timedOut:  make(chan struct{}),
		idleTimer: make(chan *time.Timer, 1),
	}
	l.idleTimer <- time.AfterFunc(l.timeout, l.timerExpired)
	return l
}

type idleListener struct {
	wrapped Listener
	timeout time.Duration

	// Only one of these channels is receivable at any given time.
	active    chan int         // count of active connections; closed when Close is called if not timed out
	timedOut  chan struct{}    // closed when the idle timer expires
	idleTimer chan *time.Timer // holds the timer only when idle
}

// Accept accepts an incoming connection.
//
// If an incoming connection is accepted concurrent to the listener being closed
// due to idleness, the new connection is immediately closed.
func (l *idleListener) Accept(ctx context.Context) (io.ReadWriteCloser, error) {
	rwc, err := l.wrapped.Accept(ctx)

	select {
	case n, ok := <-l.active:
		if err != nil {
			if ok {
				l.active <- n
			}
			return nil, err
		}
		if ok {
			l.active <- n + 1
		} else {
		}
		return l.newConn(rwc), nil

	case <-l.timedOut:
		if err == nil {
			// Keeping the connection open would leave the listener simultaneously active and closed due to idleness.
			rwc.Close()
		} else {
		}
		return nil, ErrIdleTimeout

	case timer := <-l.idleTimer:
		if err != nil {
			// The idle timer doesn't run until it receives itself from the idleTimer channel.
			l.idleTimer <- timer
			return nil, err
		}

		if !timer.Stop() {
			// Failed to stop the timer — the timer goroutine is in the process of firing.
			l.idleTimer <- timer
			rwc.Close()
			<-l.timedOut
			return nil, ErrIdleTimeout
		}

		l.active <- 1
		return l.newConn(rwc), nil
	}
}

func (l *idleListener) Close() error {
	select {
	case _, ok := <-l.active:
		if ok {
			close(l.active)
		}

	case <-l.timedOut:
		// Already closed by the timer; take care not to double-close if the caller only explicitly invokes this Close method once.
		return ErrIdleTimeout

	case timer := <-l.idleTimer:
		if !timer.Stop() {
			// Couldn't stop the timer.
			l.idleTimer <- timer
			<-l.timedOut
			return ErrIdleTimeout
		}
		close(l.active)
	}

	return l.wrapped.Close()
}

func (l *idleListener) Dialer() Dialer {
	return l.wrapped.Dialer()
}

func (l *idleListener) timerExpired() {
	select {
	case n, ok := <-l.active:
		if ok {
			panic(fmt.Sprintf("jsonrpc2: idleListener idle timer fired with %d connections still active", n))
		} else {
			panic("jsonrpc2: Close finished with idle timer still running")
		}

	case <-l.timedOut:
		panic("jsonrpc2: idleListener idle timer fired more than once")

	case <-l.idleTimer:
	}

	// Close the Listener with all channels still blocked to ensure that this call to l.wrapped.Close doesn't race with the one in l.Close.
	defer close(l.timedOut)
	l.wrapped.Close()
}

func (l *idleListener) connClosed() {
	select {
	case n, ok := <-l.active:
		if !ok {
			// l is already closed.
			return
		}
		n--
		if n == 0 {
			l.idleTimer <- time.AfterFunc(l.timeout, l.timerExpired)
		} else {
			l.active <- n
		}

	case <-l.timedOut:
		panic("jsonrpc2: idleListener idle timer fired before last active connection was closed")

	case <-l.idleTimer:
		panic("jsonrpc2: idleListener idle timer active before last active connection was closed")
	}
}

type idleListenerConn struct {
	wrapped   io.ReadWriteCloser
	l         *idleListener
	closeOnce sync.Once
}

func (l *idleListener) newConn(rwc io.ReadWriteCloser) *idleListenerConn {
	c := &idleListenerConn{
		wrapped: rwc,
		l:       l,
	}

	// A caller that forgets to call Close may disrupt the idleListener's
	// accounting, even though the file descriptor.
	runtime.SetFinalizer(c, func(c *idleListenerConn) {
		panic("jsonrpc2: IdleListener connection became unreachable without a call to Close")
	})

	return c
}

func (c *idleListenerConn) Read(p []byte) (int, error)  { return c.wrapped.Read(p) }
func (c *idleListenerConn) Write(p []byte) (int, error) { return c.wrapped.Write(p) }

func (c *idleListenerConn) Close() error {
	defer c.closeOnce.Do(func() {
		c.l.connClosed()
		runtime.SetFinalizer(c, nil)
	})
	return c.wrapped.Close()
}
