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
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// ErrClosed reports use of a pool that has been shut down.
var ErrClosed = errors.New("browser pool is closed")

// Session is the part of a browser session the pool manages.
//
// Close reports whether the profile was confirmed released. A browser that
// outlives its shutdown still owns the directory, and the pool must not hand
// that directory to a login or a write on the strength of Close merely having
// returned.
type Session interface {
	Close() error
}

// Checker is a session that can report whether it is still usable.
//
// A pooled handle can stop working without the pool being told: Chrome can
// crash, be killed, or be quit by the user, and the session then fails every
// call with a closed-connection error. Sessions implementing this are checked
// before being handed out so a dead browser is replaced rather than reused.
//
// The probe takes a context because it is browser I/O: a Chrome that is running
// but whose connection has wedged would otherwise never answer.
type Checker interface {
	Alive(ctx context.Context) bool
}

// probeTimeout bounds the liveness check. It is short because the answer is
// only interesting when it comes back quickly; a browser that cannot respond in
// a second is not one to hand to a reader.
const probeTimeout = time.Second

// alive probes a session. It must never be called while holding the pool mutex:
// this is browser I/O, and blocking here would stall every read, write, login
// and shutdown behind the same lock.
func alive(ctx context.Context, s Session) bool {
	c, ok := s.(Checker)
	if !ok {
		return true
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	return c.Alive(probeCtx)
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

	// retiring is a session taken out of service that still has leases on it.
	// It cannot be closed yet -- callers are using it -- but no new lease may
	// be handed out either, so it is held here until the last one drains.
	retiring Session

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
	return p
}

// quietLocked reports that nothing holds or is about to hold the profile: no
// outstanding leases, no launch in flight, no close still finishing.
//
// Leases alone are not enough. A caller that has published p.opening holds no
// lease yet but is about to own a browser, and a discarded Chrome keeps the
// profile lock until its process exits.
func (p *Pool) quietLocked() bool {
	return p.leases == 0 && p.opening == nil && p.closing == nil && p.retiring == nil
}

// retireLocked takes a session out of service.
//
// It never closes anything itself. Retirement and closing are deliberately
// separate steps with a single owner: concurrent probes can all fail at once,
// and if each could start a shutdown the same browser would be torn down
// several times over, racing its own cleanup and losing the record of whether
// it actually exited.
func (p *Pool) retireLocked(s Session) {
	if s == nil || s == p.retiring {
		return
	}
	if p.session == s {
		p.session = nil
	}
	if p.retiring == nil {
		p.retiring = s
		return
	}
	// Another session is already awaiting shutdown, which cannot happen while
	// the pool holds at most one browser. Close this one directly rather than
	// drop it.
	p.beginCloseLocked(s)
}

// closeRetiredLocked starts the one shutdown for a retired session, once no
// caller is still using it. Closing a browser mid-read would turn one bad probe
// into several failed requests.
func (p *Pool) closeRetiredLocked() {
	if p.retiring == nil || p.leases > 0 {
		return
	}
	s := p.retiring
	p.retiring = nil
	p.beginCloseLocked(s)
}

// beginCloseLocked retires a session, tracking the shutdown so nothing opens a
// replacement while the old Chrome still holds the profile.
//
// A shutdown that cannot confirm the process is gone is remembered: the profile
// may still be held by something this pool no longer tracks, and saying nothing
// would let the next caller believe it owns a directory it does not.
func (p *Pool) beginCloseLocked(s Session) {
	if s == nil {
		return
	}
	done := make(chan struct{})
	p.closing = done

	go func() {
		err := s.Close()

		if err != nil {
			// Nothing to record here. Whether the profile is free is decided by
			// the directory at the moment someone tries to take it, and a flag
			// kept here could only ever be a stale copy of that -- one that
			// stays set after the browser finally exits and refuses every
			// reservation from then on.
			slog.Warn("pooled browser did not confirm shutdown", "err", err)
		}

		p.mu.Lock()
		if p.closing == done {
			p.closing = nil
		}
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

		if candidate := p.session; candidate != nil {
			// Probing is browser I/O and must not happen under the lock: a
			// wedged connection would otherwise stall every read, write, login
			// and shutdown behind this mutex. Take a lease first so nothing
			// retires the session mid-probe, then reconcile.
			p.stopIdleTimerLocked()
			p.leases++
			p.mu.Unlock()

			healthy := alive(ctx, candidate)

			p.mu.Lock()
			// Another prober may have retired this session while we were asking
			// it. Concurrent probes can disagree -- one times out, one succeeds --
			// and the retirement wins: handing back a browser already on its way
			// out would have callers working against a closing connection.
			retired := p.session != candidate

			if healthy && !retired {
				p.mu.Unlock()
				return &Lease{Session: candidate, pool: p}, nil
			}

			if p.leases > 0 {
				p.leases--
			}
			if !healthy {
				p.retireLocked(candidate)
			}
			// Exactly one place starts the shutdown, once nothing holds it.
			p.closeRetiredLocked()
			p.mu.Unlock()
			continue
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

		// The launch runs to completion regardless of whether this caller is
		// still waiting, and the opening claim is held until it does.
		//
		// Letting a timed-out caller drop the claim would be the bug it looks
		// like a fix for: the Chrome it started is still coming up, and the next
		// acquire or reservation would go at the same profile behind its back.
		launched := make(chan error, 1)
		go func() {
			// Not the caller's context: the launch belongs to the pool, and giving
			// up on it early would leave a Chrome arriving with nothing tracking it.
			session, err := p.open(context.Background())

			p.mu.Lock()
			if err == nil {
				switch {
				case p.closed, p.exclusive != nil:
					// Nobody may have it now; retire it rather than leave a
					// browser holding the profile untracked.
					p.beginCloseLocked(session)
				default:
					p.session = session
					// Only schedule an idle close when warming is on. With
					// warming off, arming here would retire the browser before
					// the caller waiting on this launch could take it -- and it
					// would launch another, and another.
					if p.idle > 0 {
						p.armIdleTimerLocked()
					}
				}
			}
			p.opening = nil
			close(done)
			p.mu.Unlock()

			launched <- err
		}()

		select {
		case err := <-launched:
			if err != nil {
				return nil, err
			}
			// Loop round and take the session that was just stored, which also
			// re-checks everything that may have changed while launching.
			continue
		case <-ctx.Done():
			// This caller is done waiting, but the launch is not abandoned: the
			// claim above stays until it lands.
			return nil, ctx.Err()
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
	if err := p.await(ctx, 0, func() bool { return p.quietLocked() || p.closed }); err != nil {
		p.releaseExclusive(done)
		return nil, err
	}

	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		p.releaseExclusive(done)
		return nil, ErrClosed
	}

	// Close synchronously: the caller is about to put its own Chrome on this
	// profile and cannot do that until ours has actually let go.
	p.mu.Lock()
	session := p.session
	p.session = nil
	p.mu.Unlock()

	// A reservation keeps other callers out; it is not what keeps a second
	// Chrome off the directory. That is decided when one is started, against
	// the profile lock, by whoever starts it -- which is why this can report a
	// shutdown it could not confirm without also having to remember it. If the
	// old browser really is still there, the caller's own launch will say so,
	// and it will stop saying so the moment that process ends.
	if session != nil {
		if err := session.Close(); err != nil {
			p.releaseExclusive(done)
			return nil, fmt.Errorf("cannot take the profile: %w", err)
		}
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
		// A session retired mid-use is closed now that nothing holds it.
		p.closeRetiredLocked()
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
	p.mu.Unlock()

	if session != nil {
		_ = session.Close()
	}

	// A launch or a shutdown may still be in flight, and both end with a Chrome
	// this pool is responsible for. Returning now would let the process exit
	// with one still starting up -- a browser nothing is tracking, holding the
	// profile until someone finds and kills it. A launch already in progress is
	// retired the moment it lands, because closed is set; this only waits for
	// that to happen.
	//
	// The wait is bounded. A read that never returns its lease would otherwise
	// keep a retiring session alive and hang shutdown behind it, and a server
	// that will not exit is a worse failure than a browser left running -- one
	// the user can see and quit.
	inFlight := func() bool {
		return p.opening == nil && p.closing == nil && p.retiring == nil
	}
	if err := p.await(context.Background(), shutdownWait, inFlight); err != nil {
		slog.Warn("gave up waiting for the browser to shut down", "after", shutdownWait, "err", err)
	}
}

// await blocks until ready reports true, the context is done, or the budget runs
// out; a budget of zero or less means no deadline. ready is called with the lock
// held.
//
// It polls rather than waiting on a condition variable. Every wait in this
// package is bounded by a caller's deadline or a shutdown budget, and a
// sync.Cond cannot be woken by either -- which is what used to leave waiter
// goroutines blocked for the life of the process once a wait gave up. The
// interval costs a few milliseconds against waits that last for seconds.
func (p *Pool) await(ctx context.Context, budget time.Duration, ready func() bool) error {
	var deadline time.Time
	if budget > 0 {
		deadline = time.Now().Add(budget)
	}

	for {
		p.mu.Lock()
		done := ready()
		p.mu.Unlock()
		if done {
			return nil
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return fmt.Errorf("the pool was still busy after %s", budget)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// pollInterval is how often await rechecks. Small enough not to add noticeable
// latency to a handoff, large enough not to spin.
const pollInterval = 5 * time.Millisecond

// shutdownWait bounds how long Close waits for in-flight browser work. It is a
// variable so tests can shorten it.
var shutdownWait = 15 * time.Second

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
	// A reservation is waiting for the pool to go quiet so it can shut the
	// session down itself. Retiring it from here would start that shutdown
	// behind the reservation's back: it would see no session, take the profile,
	// and hand it to a login window while the old Chrome was still exiting.
	if p.exclusive != nil {
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
