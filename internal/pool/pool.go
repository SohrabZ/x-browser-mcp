// Package pool keeps one browser alive across reads instead of launching a new
// one for each.
//
// Measured on a real read, launching Chrome costs 0.83s, quitting it costs
// 1.00s, and a cold browser loads a page in 2.18s against 0.38s warm. Reuse
// therefore saves roughly 3.2s of a 4.5s read.
//
// The constraint that shapes everything here is the profile lock: only one
// Chrome may hold a user-data-dir. A warm browser is holding it, so anything
// else that needs the profile -- the interactive login window, a write running
// in a visible browser -- must be able to take it back. That is what Evict is
// for, and why leases are counted rather than assumed.
package pool

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrClosed reports use of a pool that has been shut down.
var ErrClosed = errors.New("browser pool is closed")

// Session is the part of a browser session the pool manages.
type Session interface {
	Close()
}

// Checker is a session that can report whether it is still usable.
//
// A pooled handle can stop working without the pool being told: Chrome can
// crash, be killed, or be quit by the user, and the session then fails every
// call with a closed-connection error. Sessions implementing this are checked
// before being handed out so a dead browser is replaced rather than reused.
type Checker interface {
	Alive() bool
}

// alive reports whether a session is usable. Sessions that cannot answer are
// assumed usable, since the pool has nothing better to go on.
func alive(s Session) bool {
	c, ok := s.(Checker)
	return !ok || c.Alive()
}

// Opener starts a new session.
type Opener func(ctx context.Context) (Session, error)

// Pool hands out a shared browser session, keeping it warm between uses.
//
// The zero idle duration disables warming entirely: each lease opens and closes
// its own session, which is the old behaviour and a useful escape hatch.
type Pool struct {
	open Opener
	idle time.Duration

	mu       sync.Mutex
	session  Session
	leases   int
	closed   bool
	idleStop chan struct{}

	// drained signals waiters when the last lease is returned.
	drained *sync.Cond

	// now and afterFunc are swappable so tests need not sleep.
	now       func() time.Time
	afterFunc func(time.Duration, func()) *time.Timer
}

// New builds a pool. A session with no outstanding leases is closed once it has
// been idle for the given duration.
func New(open Opener, idle time.Duration) *Pool {
	p := &Pool{
		open:      open,
		idle:      idle,
		now:       time.Now,
		afterFunc: time.AfterFunc,
	}
	p.drained = sync.NewCond(&p.mu)
	return p
}

// Lease is a borrowed session. Release must be called exactly once.
type Lease struct {
	Session Session

	pool *Pool
	once sync.Once
}

// Release returns the session to the pool.
func (l *Lease) Release() {
	if l == nil || l.pool == nil {
		return
	}
	l.once.Do(func() { l.pool.release() })
}

// Acquire borrows the shared session, opening one if none is warm.
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, ErrClosed
	}
	p.stopIdleTimerLocked()

	// A pooled browser can die without the pool being told -- a crash, an OOM
	// kill, or the user quitting it -- and then fails every call. Drop a dead
	// one instead of handing it out.
	var dead Session
	if p.session != nil && !alive(p.session) {
		dead, p.session = p.session, nil
	}

	if p.session != nil {
		p.leases++
		session := p.session
		p.mu.Unlock()
		return &Lease{Session: session, pool: p}, nil
	}
	p.mu.Unlock()

	if dead != nil {
		dead.Close()
	}

	// Opening is slow and must not hold the lock, or every caller queues behind
	// the first one's browser launch.
	session, err := p.open(ctx)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	switch {
	case p.closed:
		p.mu.Unlock()
		session.Close()
		return nil, ErrClosed
	case p.session != nil:
		// Another caller won the race; keep theirs and discard this one so the
		// profile is never held by two browsers.
		p.leases++
		winner := p.session
		p.mu.Unlock()
		session.Close()
		return &Lease{Session: winner, pool: p}, nil
	default:
		p.session = session
		p.leases++
		p.mu.Unlock()
		return &Lease{Session: session, pool: p}, nil
	}
}

func (p *Pool) release() {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.leases > 0 {
		p.leases--
	}
	if p.leases == 0 {
		p.drained.Broadcast()
		p.armIdleTimerLocked()
	}
}

// Evict closes the warm session so something else can take the profile.
//
// It waits for outstanding leases to be returned rather than yanking a browser
// out from under an in-flight read. The context bounds that wait.
func (p *Pool) Evict(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		p.mu.Lock()
		for p.leases > 0 {
			p.drained.Wait()
		}
		p.mu.Unlock()
		close(done)
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}

	p.mu.Lock()
	p.stopIdleTimerLocked()
	session := p.session
	p.session = nil
	p.mu.Unlock()

	if session != nil {
		session.Close()
	}
	return nil
}

// Close shuts the pool down. Further calls to Acquire fail.
func (p *Pool) Close() {
	p.mu.Lock()
	p.closed = true
	p.stopIdleTimerLocked()
	session := p.session
	p.session = nil
	p.drained.Broadcast()
	p.mu.Unlock()

	if session != nil {
		session.Close()
	}
}

// Warm reports whether a session is currently held.
func (p *Pool) Warm() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.session != nil
}

// armIdleTimerLocked schedules the idle close. The caller holds the lock.
//
// An idle Chrome costs memory, and a browser that lives for days is a larger
// surface than one that lives for minutes, so warmth is deliberately temporary.
func (p *Pool) armIdleTimerLocked() {
	if p.idle <= 0 || p.session == nil || p.closed {
		if p.idle <= 0 {
			// Warming disabled: close immediately rather than hold the profile.
			session := p.session
			p.session = nil
			if session != nil {
				go session.Close()
			}
		}
		return
	}

	stop := make(chan struct{})
	p.idleStop = stop

	p.afterFunc(p.idle, func() {
		select {
		case <-stop:
			return // superseded by a new lease
		default:
		}

		p.mu.Lock()
		if p.leases > 0 || p.idleStop != stop {
			p.mu.Unlock()
			return
		}
		session := p.session
		p.session = nil
		p.idleStop = nil
		p.mu.Unlock()

		if session != nil {
			session.Close()
		}
	})
}

func (p *Pool) stopIdleTimerLocked() {
	if p.idleStop != nil {
		close(p.idleStop)
		p.idleStop = nil
	}
}
