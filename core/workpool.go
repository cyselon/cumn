package core

import (
	"errors"
	"net"
	"runtime"
	"strings"
	"sync"
	"time"
)

type WorkerPool struct {
	Handler ServeHandler

	MaxWorkersCount int
	workersCount    int

	MaxIdleWorkerDuration time.Duration

	LogAllErrors bool
	Logger       Logger

	lock sync.Mutex

	stopCh   chan struct{}
	mustStop bool
	ready    []*workerChan

	workerChanPool sync.Pool

	connState func(net.Conn, ConnState)
}

type workerChan struct {
	lastUseTime time.Time
	ch          chan net.Conn
}

var workerChanCap = func() int {
	// Use blocking workerChan if GOMAXPROCS=1.
	// This immediately switches Serve to WorkerFunc, which results
	// in higher performance (under go1.5 at least).
	if runtime.GOMAXPROCS(0) == 1 {
		return 0
	}

	// Use non-blocking workerChan if GOMAXPROCS>1,
	// since otherwise the Serve caller (Acceptor) may lag accepting
	// new connections if WorkerFunc is CPU-bound.
	return 1
}()

func (wp *WorkerPool) Stop() error {
	if wp.stopCh == nil {
		return errors.New("worker pool not started yet")
	}
	close(wp.stopCh)
	wp.stopCh = nil

	// Stop all the workers waiting for incoming connections.
	// Do not wait for busy workers - they will stop after
	// serving the connection and noticing wp.mustStop = true.
	wp.lock.Lock()
	defer wp.lock.Unlock()
	ready := wp.ready
	for i, ch := range ready {
		ch.ch <- nil
		ready[i] = nil
	}
	wp.ready = ready[:0]
	wp.mustStop = true
	return nil
}

func (wp *WorkerPool) Start() error {
	if wp.stopCh != nil {
		return errors.New("worker pool already started")
	}
	wp.stopCh = make(chan struct{})
	stopCh := wp.stopCh
	go func() {
		var scratch []*workerChan
		for {
			wp.clean(&scratch)
			select {
			case <-stopCh:
				return
			default:
				time.Sleep(wp.getMaxIdleWorkerDuration())
			}
		}
	}()
	return nil
}

func (wp *WorkerPool) getMaxIdleWorkerDuration() time.Duration {
	if wp.MaxIdleWorkerDuration <= 0 {
		return 10 * time.Second
	}
	return wp.MaxIdleWorkerDuration
}

func (wp *WorkerPool) clean(scratch *[]*workerChan) {
	maxIdleWorkerDuration := wp.getMaxIdleWorkerDuration()
	currentTime := time.Now()
	wp.lock.Lock()
	ready := wp.ready
	n := len(ready)
	i := 0
	for i < n && currentTime.Sub(ready[i].lastUseTime) > maxIdleWorkerDuration {
		i++
	}
	*scratch = append((*scratch)[:0], ready[:i]...)
	if i > 0 {
		m := copy(ready, ready[i:])
		for i = m; i < n; i++ {
			ready[i] = nil
		}
		wp.ready = ready[:m]
	}
	wp.lock.Unlock()

	// Notify obsolete workers to stop.
	// This notification must be outside the wp.lock, since ch.ch
	// may be blocking and may consume a lot of time if many workers
	// are located on non-local CPUs.
	tmp := *scratch
	for i, ch := range tmp {
		ch.ch <- nil
		tmp[i] = nil
	}
}

func (wp *WorkerPool) getCh() (ch *workerChan) {
	createWorker := false

	wp.lock.Lock()
	ready := wp.ready
	n := len(ready) - 1
	if n < 0 {
		if wp.workersCount < wp.MaxWorkersCount {
			createWorker = true
			wp.workersCount++
		}
	} else {
		ch = ready[n]
		ready[n] = nil
		wp.ready = ready[:n]
	}
	wp.lock.Unlock()

	if ch == nil && createWorker {
		vch := wp.workerChanPool.Get()
		if vch == nil {
			vch = &workerChan{
				ch: make(chan net.Conn, workerChanCap),
			}
		}
		ch = vch.(*workerChan)
		go func() {
			wp.workerFunc(ch)
			wp.workerChanPool.Put(vch)
		}()
	}
	return ch
}

func (wp *WorkerPool) Serve(c net.Conn) bool {
	ch := wp.getCh()
	if ch == nil {
		return false
	}
	ch.ch <- c
	return true
}

func (wp *WorkerPool) workerFunc(ch *workerChan) {
	var c net.Conn
	var err error
	for c = range ch.ch {
		if c == nil {
			break
		}
		if err = wp.Handler(c); err != nil && err != errHijacked {
			errStr := err.Error()
			if wp.LogAllErrors || !(strings.Contains(errStr, "broken pipe") ||
				strings.Contains(errStr, "reset by peer") ||
				strings.Contains(errStr, "request headers: small read buffer") ||
				strings.Contains(errStr, "i/o timeout")) {
				wp.Logger.Printf("error when serving connection %q<->%q: %s", c.LocalAddr(), c.RemoteAddr(), err)
			}
			if err == errHijacked {
				wp.connState(c, StateHijacked)
			} else {
				_ = c.Close()
				wp.connState(c, StateClosed)
			}
			c = nil
		}
		if !wp.release(ch) {
			break
		}
	}

	wp.lock.Lock()
	wp.workersCount--
	wp.lock.Unlock()
}

func (wp *WorkerPool) release(ch *workerChan) bool {
	ch.lastUseTime = time.Now()
	wp.lock.Lock()
	defer wp.lock.Unlock()
	if wp.mustStop {
		return false
	}
	wp.ready = append(wp.ready, ch)
	return true
}
