// Package pool keeps one browser alive across reads instead of launching a new
// one for each.
//
// Measured on a real read, launching Chrome costs 0.83s, quitting it costs
// 1.00s, and a cold browser loads a page more slowly than a warm one. Reuse cut
// the median uncached read from 3.58s to 2.02s.
//
// The constraint that shapes everything here is the profile lock: exactly one
// Chrome may hold a user-data-dir. That makes the profile a resource with a
// single owner, so the pool has to guarantee two things that a plain cache does
// not:
//
//   - Only one browser is ever launched at a time. Two concurrent launches both
//     pass Chrome's own profile-in-use check and then contend for the directory.
//   - Something outside the pool -- an interactive login, a write in a visible
//     browser -- can take the profile and hold it, with reads kept out for the
//     whole time rather than only at the moment of handover.
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

func alive(s Session) bool {
	c, ok := s.(Checker)
	return !ok || c.Alive()
}

// Opener starts a new session.
type Opener func(ctx context.Context) (Session, error)

// Reservation is exclusive use of the profile, held by something outside the
// pool. Release must be called, or reads stay blocked.
type Reservation interface {
	Release()
}

// Reserver hands out exclusive use of the profile.
type Reserver interface {
	Reserve(ctx context.Context) (Reservation, error)
}

// Pool hands out a shared browser session, keeping it warm between uses.
type Pool struct {
	open Opener
	idle time.Duration

	mu       sync.Mutex
	session  Session
	leases   int
	closed   bool
	idleStop chan struct{}

	// opening is non-nil while one caller is launching a browser. Others wait
	// on it rather than launching their own.
	opening chan struct{}

	// closing is non-nil while a discarded browser is still shutting down.
	//
	// Chrome holds the profile lock until it has actually exited, so starting a
	// replacement before the old one finishes puts two processes on the
	// directory -- the failure the whole package exists to prevent. Closing is
	// slow enough (about a second) that it cannot block the caller, so it is
	// tracked instead.
	closing chan struct{}

	// exclusive is non-nil while the profile is reserved outside the pool. It
	// is closed on release, which is what waiting acquires block on.
	exclusive chan struct{}

	// drained wakes waiters when the last lease is returned.
	drained *sync.Cond

	now       func() time.Time
	afterFunc func(time.Duration, func()) *time.Timer
}

// New builds a pool. A session with no outstanding leases is closed once it has
// been idle for the given duration; zero disables warming entirely.
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

// quietLocked reports that nothing holds or is about to hold the profile: no
// outstanding leases, no launch in flight, no close still finishing.
//
// Leases alone are not enough. A caller that has published p.opening holds no
// lease yet but is about to own a browser, and a discarded Chrome keeps the
// profile lock until its process exits.
func (p *Pool) quietLocked() bool {
	return p.leases == 0 && p.opening == nil && p.closing == nil
}

// beginCloseLocked retires a session, tracking the shutdown so nothing opens a
// replacement while the old Chrome still holds the profile.
func (p *Pool) beginCloseLocked(s Session) {
	if s == nil {
		return
	}
	done := make(chan struct{})
	p.closing = done

	go func() {
		s.Close()
		p.mu.Lock()
		if p.closing == done {
			p.closing = nil
		}
		p.drained.Broadcast()
		p.mu.Unlock()
		close(done)
	}()
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
//
// It blocks while the profile is reserved and while another caller is already
// launching, rather than proceeding in parallel: both would put a second Chrome
// on a directory that allows one.
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, ErrClosed
		}

		// Something outside the pool owns the profile. Wait for it rather than
		// failing: a write holds it for seconds, and a queued read is better
		// than a spurious error.
		if wait := p.exclusive; wait != nil {
			p.mu.Unlock()
			if err := waitFor(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}

		// A retiring browser still owns the profile until it exits.
		if wait := p.closing; wait != nil {
			p.mu.Unlock()
			if err := waitFor(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}

		// A pooled browser can die without the pool being told -- a crash, an
		// OOM kill, or the user quitting it -- and then fails every call.
		// Alive cannot distinguish death from a transient fault, so the old
		// process is retired properly rather than abandoned.
		if p.session != nil && !alive(p.session) {
			dead := p.session
			p.session = nil
			p.beginCloseLocked(dead)
			p.mu.Unlock()
			continue
		}

		if p.session != nil {
			p.stopIdleTimerLocked()
			p.leases++
			session := p.session
			p.mu.Unlock()
			return &Lease{Session: session, pool: p}, nil
		}

		// Someone else is already launching; take their result.
		if wait := p.opening; wait != nil {
			p.mu.Unlock()
			if err := waitFor(ctx, wait); err != nil {
				return nil, err
			}
			continue
		}

		// Become the opener. Holding the lock across a browser launch would
		// stall every other caller, so the claim is published instead.
		done := make(chan struct{})
		p.opening = done
		p.mu.Unlock()

		session, err := p.open(ctx)

		p.mu.Lock()
		p.opening = nil
		close(done)
		// A reservation may be waiting on this launch to finish.
		p.drained.Broadcast()

		switch {
		case err != nil:
			p.mu.Unlock()
			return nil, err
		case p.closed:
			p.beginCloseLocked(session)
			p.mu.Unlock()
			return nil, ErrClosed
		case p.exclusive != nil:
			// Reserved while this was launching. Retire it and wait like
			// everyone else; the reservation waits for that close to finish.
			p.beginCloseLocked(session)
			p.mu.Unlock()
			continue
		case ctx.Err() != nil:
			// The caller gave up mid-launch. The browser is fine, so keep it
			// warm for the next read rather than discarding a good launch.
			p.session = session
			p.armIdleTimerLocked()
			p.mu.Unlock()
			return nil, ctx.Err()
		default:
			p.session = session
			p.stopIdleTimerLocked()
			p.leases++
			p.mu.Unlock()
			return &Lease{Session: session, pool: p}, nil
		}
	}
}

// Reserve takes exclusive use of the profile for something outside the pool.
//
// New acquires are blocked first, then outstanding leases are waited out, then
// the warm browser is closed. Blocking first is the point: a reservation that
// only closed the current browser would let the next read open another one
// while the login window or a write still had the profile.
func (p *Pool) Reserve(ctx context.Context) (Reservation, error) {
	p.mu.Lock()
	for {
		if p.closed {
			p.mu.Unlock()
			return nil, ErrClosed
		}
		wait := p.exclusive
		if wait == nil {
			break
		}
		p.mu.Unlock()
		if err := waitFor(ctx, wait); err != nil {
			return nil, err
		}
		p.mu.Lock()
	}

	done := make(chan struct{})
	p.exclusive = done
	p.stopIdleTimerLocked()
	p.mu.Unlock()

	// With acquires blocked, wait for the profile to go quiet. Leases alone are
	// not enough: a caller may already be launching a browser without holding
	// one, and a retiring Chrome keeps the profile lock until it exits.
	drained := make(chan struct{})
	go func() {
		p.mu.Lock()
		for !p.quietLocked() && !p.closed {
			p.drained.Wait()
		}
		p.mu.Unlock()
		close(drained)
	}()

	select {
	case <-ctx.Done():
		p.releaseExclusive(done)
		return nil, ctx.Err()
	case <-drained:
	}

	// Close synchronously: the caller is about to put its own Chrome on this
	// profile and cannot do that until ours has actually let go.
	p.mu.Lock()
	session := p.session
	p.session = nil
	p.mu.Unlock()

	if session != nil {
		session.Close()
	}
	return &exclusive{pool: p, done: done}, nil
}

// exclusive is a held reservation.
type exclusive struct {
	pool *Pool
	done chan struct{}
	once sync.Once
}

func (e *exclusive) Release() {
	e.once.Do(func() { e.pool.releaseExclusive(e.done) })
}

func (p *Pool) releaseExclusive(done chan struct{}) {
	p.mu.Lock()
	if p.exclusive == done {
		p.exclusive = nil
	}
	p.drained.Broadcast()
	p.mu.Unlock()
	close(done)
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
	if p.closed {
		return
	}
	if p.idle <= 0 {
		// Warming disabled: retire the browser, but track the shutdown so the
		// next read does not start a replacement while this one still holds the
		// profile.
		session := p.session
		p.session = nil
		p.beginCloseLocked(session)
		return
	}
	if p.session == nil {
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
		p.beginCloseLocked(session)
		p.mu.Unlock()
	})
}

func (p *Pool) stopIdleTimerLocked() {
	if p.idleStop != nil {
		close(p.idleStop)
		p.idleStop = nil
	}
}

// waitFor blocks until ch closes or ctx ends.
func waitFor(ctx context.Context, ch <-chan struct{}) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}
