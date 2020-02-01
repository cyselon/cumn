package core

import (
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrAlreadyServing is returned when calling Serve on a Server
	// that is already serving connections.
	ErrAlreadyServing   = errors.New("server is already serving connections")
	ErrConcurrencyLimit = errors.New("empty concurrency limit")
	errHijacked         = errors.New("connection has been hijacked")
)

const DefaultConcurrency = 1024

type ServeHandler func(c net.Conn) error

// Logger is used for logging formatted messages.
type Logger interface {
	// Printf must have the same semantics as log.Printf.
	Printf(format string, args ...interface{})
}

type Server struct {
	noCopy noCopy

	Concurrency   int
	MaxConnsPerIP int

	ln net.Listener

	maxIdlDuration                     time.Duration
	SleepWhenConcurrencyLimitsExceeded time.Duration

	handler ServeHandler

	mu            sync.Mutex
	done          chan struct{}
	concurrencyCh chan struct{}

	open int32
	stop int32

	concurrency uint32
}

func (s *Server) Serve(listener net.Listener) error {
	var lastOverflowErrorTime time.Time
	var lastPerIPErrorTime time.Time
	var c net.Conn
	var err error

	s.mu.Lock()
	{
		if s.ln != nil {
			s.mu.Unlock()
			return ErrAlreadyServing
		}
		s.ln = listener
		s.done = make(chan struct{})
	}
	s.mu.Unlock()

	// initial wait group
	maxWorkerCount := s.getConcurrency()
	s.concurrencyCh = make(chan struct{}, maxWorkerCount)
	wp := &WorkerPool{
		Handler:               s.serveConn,
		MaxWorkersCount:       maxWorkerCount,
		workersCount:          0,
		MaxIdleWorkerDuration: s.maxIdlDuration,
		mustStop:              false,
	}
	_ = wp.Start()

	atomic.AddInt32(&s.open, 1)
	defer atomic.AddInt32(&s.open, -1)

	// wait for coming client connections
	for {
		if c, err = s.acceptConn(listener, &lastPerIPErrorTime); err != nil {
			_ = wp.Stop()
			if err == io.EOF {
				return nil
			}
			return err
		}

		atomic.AddInt32(&s.open, 1)
		if !wp.Serve(c) {
			atomic.AddInt32(&s.open, -1)
			_ = c.Close()
			if time.Since(lastOverflowErrorTime) > time.Minute {
				// TODO: log error
				lastOverflowErrorTime = time.Now()
			}
			if s.SleepWhenConcurrencyLimitsExceeded > 0 {
				time.Sleep(s.SleepWhenConcurrencyLimitsExceeded)
			}
		}
		c = nil
	}
}

func (s *Server) Shutdown() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.StoreInt32(&s.stop, 1)
	defer atomic.StoreInt32(&s.stop, 0)

	if s.ln == nil {
		return nil
	}
	if err := s.ln.Close(); err != nil {
		return err
	}

	if s.done != nil {
		close(s.done)
	}

	for {
		if open := atomic.LoadInt32(&s.open); open == 0 {
			break
		}
		time.Sleep(time.Millisecond * 10)
	}
	s.ln = nil
	return nil
}

func (s *Server) ServeConn(c net.Conn) error {
	if s.MaxConnsPerIP > 0 {
		// TODO: max connections per ip limit
	}
	n := atomic.AddUint32(&s.concurrency, ^uint32(0))
	if n > uint32(s.getConcurrency()) {
		atomic.AddUint32(&s.concurrency, 1)
		_ = c.Close()
		return ErrConcurrencyLimit
	}

	atomic.AddInt32(&s.open, 1)
	err := s.serveConn(c)
	atomic.AddUint32(&s.concurrency, ^uint32(0))
	if err != errHijacked {
		err1 := c.Close()
		if err1 == nil {
			err = nil
		}
	} else {
		err = nil
	}
	return err
}

func (s *Server) getConcurrency() int {
	n := s.Concurrency
	if n <= 0 {
		n = DefaultConcurrency
	}
	return n
}

func (s *Server) acceptConn(ln net.Listener, lastErrTime *time.Time) (net.Conn, error) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if c != nil {
				return nil, errors.New("BUG: accept return non-nil connection on non-nil error")
			}
			if netErr, ok := err.(net.Error); ok && netErr.Temporary() {
				time.Sleep(time.Millisecond * 500)
				continue
			}
			if err != io.EOF && !strings.Contains(err.Error(), "use of closed network connection") {
				return nil, err
			}
			return nil, io.EOF
		}
		if c == nil {
			panic("BUG: accept return (nil,nil)")
		}
		if s.MaxConnsPerIP > 0 {
			// TODO: implement this max connection limit
		}
		return c, nil
	}
}

func (s *Server) serveConn(c net.Conn) error {
	if s.handler == nil {
		return nil
	}
	return s.handler(c)
}
