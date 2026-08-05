package pool

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSession records whether it was closed, and can report itself dead.
type fakeSession struct {
	id     int
	closed atomic.Bool
	dead   atomic.Bool
}

func (f *fakeSession) Close() error { f.closed.Store(true); return nil }

func (f *fakeSession) Alive(context.Context) bool { return !f.dead.Load() }

// counting returns an opener that hands out numbered sessions and counts how
// many browsers were launched -- the number this whole package exists to reduce.
func counting() (Opener, *atomic.Int64, *[]*fakeSession) {
	var n atomic.Int64
	var mu sync.Mutex
	made := make([]*fakeSession, 0, 4)

	return func(context.Context) (Session, error) {
		id := int(n.Add(1))
		s := &fakeSession{id: id}
		mu.Lock()
		made = append(made, s)
		mu.Unlock()
		return s, nil
	}, &n, &made
}

func TestSecondAcquireReusesTheWarmSession(t *testing.T) {
	open, launches, _ := counting()
	p := New(open, time.Minute)
	defer p.Close()

	first, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	first.Release()

	second, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	defer second.Release()

	if got := launches.Load(); got != 1 {
		t.Fatalf("expected one browser launch, got %d", got)
	}
	if first.Session != second.Session {
		t.Error("the second lease should hand back the same warm session")
	}
}

// Only one Chrome may hold a user-data-dir, so concurrent cold acquires must
// produce exactly one launch -- not several that are reconciled afterwards.
// Two launches both pass Chrome's own profile-in-use check and then contend for
// the directory, which is the failure this package exists to prevent.
func TestConcurrentColdAcquiresLaunchExactlyOneBrowser(t *testing.T) {
	var concurrent, peak, launches atomic.Int64

	p := New(func(context.Context) (Session, error) {
		n := concurrent.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		launches.Add(1)
		time.Sleep(20 * time.Millisecond) // a launch is slow
		concurrent.Add(-1)
		return &fakeSession{}, nil
	}, time.Minute)
	defer p.Close()

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := p.Acquire(t.Context())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			l.Release()
		}()
	}
	wg.Wait()

	if got := peak.Load(); got > 1 {
		t.Fatalf("%d browsers were launched at once; the profile allows one", got)
	}
	if got := launches.Load(); got != 1 {
		t.Fatalf("expected exactly one launch for concurrent cold acquires, got %d", got)
	}
}

func TestReleaseKeepsTheSessionWarm(t *testing.T) {
	open, _, made := counting()
	p := New(open, time.Minute)
	defer p.Close()

	l, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.Release()

	if !p.Warm() {
		t.Fatal("releasing a lease must not close the browser")
	}
	if (*made)[0].closed.Load() {
		t.Fatal("session was closed on release")
	}
}

// A warm browser holds the profile lock, so the login window and any visible
// write need it handed back.
func TestReserveClosesTheSession(t *testing.T) {
	open, _, made := counting()
	p := New(open, time.Minute)
	defer p.Close()

	l, _ := p.Acquire(t.Context())
	l.Release()

	res, err := p.Reserve(t.Context())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	defer res.Release()

	if p.Warm() {
		t.Fatal("the pool should hold nothing while the profile is reserved")
	}
	if !(*made)[0].closed.Load() {
		t.Fatal("the session should have been closed for the reservation")
	}
}

// The reservation is the whole point: while something outside the pool holds
// the profile, no read may start a second browser on it. Releasing the current
// browser and letting the next read open another would recreate the collision.
func TestReservationBlocksAcquireUntilReleased(t *testing.T) {
	open, launches, _ := counting()
	p := New(open, time.Minute)
	defer p.Close()

	res, err := p.Reserve(t.Context())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		l, err := p.Acquire(context.Background())
		if err == nil {
			l.Release()
		}
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("a read got the profile while it was reserved")
	case <-time.After(80 * time.Millisecond):
	}
	if launches.Load() != 0 {
		t.Fatalf("no browser may be launched while reserved, got %d", launches.Load())
	}

	res.Release()

	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("the read never resumed after release")
	}
}

// Reserving must not close a browser that a read has just taken.
func TestReserveDoesNotStealAnActiveLease(t *testing.T) {
	open, _, made := counting()
	p := New(open, time.Minute)
	defer p.Close()

	l, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	reserved := make(chan struct{})
	go func() {
		res, err := p.Reserve(context.Background())
		if err == nil {
			res.Release()
		}
		close(reserved)
	}()

	time.Sleep(50 * time.Millisecond)
	if (*made)[0].closed.Load() {
		t.Fatal("the session was closed while a lease was outstanding")
	}

	l.Release()
	select {
	case <-reserved:
	case <-time.After(2 * time.Second):
		t.Fatal("reserve never completed")
	}
}

// Two reservations must not overlap, or a login and a write would both believe
// they own the profile.
func TestReservationsAreExclusiveOfEachOther(t *testing.T) {
	open, _, _ := counting()
	p := New(open, time.Minute)
	defer p.Close()

	first, err := p.Reserve(t.Context())
	if err != nil {
		t.Fatalf("first reserve: %v", err)
	}

	second := make(chan struct{})
	go func() {
		r, err := p.Reserve(context.Background())
		if err == nil {
			r.Release()
		}
		close(second)
	}()

	select {
	case <-second:
		t.Fatal("a second reservation was granted while the first was held")
	case <-time.After(80 * time.Millisecond):
	}

	first.Release()
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("the second reservation never completed")
	}
}

// Reserving must not yank a browser out from under an in-flight read.
func TestReserveWaitsForOutstandingLeases(t *testing.T) {
	open, _, _ := counting()
	p := New(open, time.Minute)
	defer p.Close()

	l, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		res, err := p.Reserve(context.Background())
		if res != nil {
			res.Release()
		}
		done <- err
	}()

	select {
	case <-done:
		t.Fatal("reserve returned while a lease was still held")
	case <-time.After(50 * time.Millisecond):
	}

	l.Release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reserve did not finish after the lease was released")
	}
}

// A read that hangs must not block a reservation forever; the caller's context
// bounds the wait.
func TestReserveHonoursItsContext(t *testing.T) {
	open, _, _ := counting()
	p := New(open, time.Minute)
	defer p.Close()

	l, _ := p.Acquire(t.Context()) // deliberately never released
	defer l.Release()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if _, err := p.Reserve(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want DeadlineExceeded", err)
	}
	// A failed reservation must not leave the profile blocked.
	l2, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire after a failed reservation: %v", err)
	}
	l2.Release()
}

func TestAcquireAfterReservationOpensAgain(t *testing.T) {
	open, launches, _ := counting()
	p := New(open, time.Minute)
	defer p.Close()

	l, _ := p.Acquire(t.Context())
	l.Release()
	res, err := p.Reserve(t.Context())
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	res.Release()

	l2, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire after reservation: %v", err)
	}
	defer l2.Release()

	if got := launches.Load(); got != 2 {
		t.Fatalf("expected a fresh launch after eviction, got %d launches", got)
	}
}

// An idle Chrome holds memory, and one that lives for days is a bigger surface
// than one that lives for minutes, so warmth expires.
func TestIdleSessionIsClosed(t *testing.T) {
	open, _, made := counting()
	p := New(open, 20*time.Millisecond)
	defer p.Close()

	l, _ := p.Acquire(t.Context())
	l.Release()

	deadline := time.After(2 * time.Second)
	for p.Warm() {
		select {
		case <-deadline:
			t.Fatal("idle session was never closed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if !(*made)[0].closed.Load() {
		t.Fatal("the idle session should have been closed")
	}
}

// Re-acquiring before the idle timer fires must cancel it, or a busy pool would
// close the browser out from under itself.
func TestIdleTimerIsCancelledByANewLease(t *testing.T) {
	open, _, _ := counting()
	p := New(open, 60*time.Millisecond)
	defer p.Close()

	l, _ := p.Acquire(t.Context())
	l.Release()

	time.Sleep(20 * time.Millisecond)
	l2, err := p.Acquire(t.Context()) // resets the idle window
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	time.Sleep(80 * time.Millisecond) // past the original deadline
	if !p.Warm() {
		t.Fatal("the session was closed despite an outstanding lease")
	}
	l2.Release()
}

// A zero idle duration turns warming off: each lease gets its own browser, which
// is the pre-pool behaviour and a useful escape hatch.
func TestZeroIdleDisablesWarming(t *testing.T) {
	open, launches, _ := counting()
	p := New(open, 0)
	defer p.Close()

	l, _ := p.Acquire(t.Context())
	l.Release()

	deadline := time.After(time.Second)
	for p.Warm() {
		select {
		case <-deadline:
			t.Fatal("a zero idle duration must not keep a session warm")
		case <-time.After(5 * time.Millisecond):
		}
	}

	l2, _ := p.Acquire(t.Context())
	l2.Release()
	if got := launches.Load(); got != 2 {
		t.Fatalf("expected a launch per lease, got %d", got)
	}
}

func TestAcquireAfterCloseFails(t *testing.T) {
	open, _, _ := counting()
	p := New(open, time.Minute)
	p.Close()

	if _, err := p.Acquire(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}

func TestOpenFailureIsReported(t *testing.T) {
	boom := errors.New("chrome missing")
	p := New(func(context.Context) (Session, error) { return nil, boom }, time.Minute)
	defer p.Close()

	if _, err := p.Acquire(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("got %v, want the opener's error", err)
	}
	if p.Warm() {
		t.Error("a failed open must not leave the pool believing it is warm")
	}
}

func TestDoubleReleaseIsSafe(t *testing.T) {
	open, _, _ := counting()
	p := New(open, time.Minute)
	defer p.Close()

	l, _ := p.Acquire(t.Context())
	l.Release()
	l.Release() // must not drop the count below zero or close a shared session

	if !p.Warm() {
		t.Fatal("a duplicate release should be a no-op")
	}
}

// A browser that dies underneath the pool must be replaced, not handed out
// again. Reusing it fails every call, and nothing recovers until the idle timer
// happens to fire.
func TestDeadSessionIsReplaced(t *testing.T) {
	var launches atomic.Int64
	first := &fakeSession{}
	first.dead.Store(true)

	p := New(func(context.Context) (Session, error) {
		if launches.Add(1) == 1 {
			return first, nil
		}
		return &fakeSession{}, nil
	}, time.Minute)
	defer p.Close()

	l1, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	l1.Release()

	l2, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	defer l2.Release()

	if l2.Session == Session(first) {
		t.Fatal("the dead session was handed out again")
	}
	// Discarded sessions are closed off the caller's path, since quitting Chrome
	// takes about a second.
	deadline := time.After(2 * time.Second)
	for !first.closed.Load() {
		select {
		case <-deadline:
			t.Fatal("the dead session should have been closed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if got := launches.Load(); got != 2 {
		t.Fatalf("expected a replacement browser, got %d launches", got)
	}
}

// Sessions that cannot report liveness are assumed usable; the pool has nothing
// better to go on and must not discard them every time.
func TestSessionWithoutLivenessCheckIsReused(t *testing.T) {
	open, launches, _ := counting()
	p := New(open, time.Minute)
	defer p.Close()

	l1, _ := p.Acquire(t.Context())
	l1.Release()
	l2, _ := p.Acquire(t.Context())
	defer l2.Release()

	if got := launches.Load(); got != 1 {
		t.Fatalf("expected reuse, got %d launches", got)
	}
}

// blockingSession lets a test hold Close open, standing in for a Chrome that
// has not finished exiting and so still holds the profile lock.
type blockingSession struct {
	fakeSession
	closeGate chan struct{}
}

func (b *blockingSession) Close() error {
	if b.closeGate != nil {
		<-b.closeGate
	}
	return b.fakeSession.Close()
}

// A caller that has claimed the launch holds no lease yet, but is about to own
// a browser. A reservation that only waited for leases would return while that
// Chrome was still starting, and the login or write would put a second one on
// the same profile.
func TestReserveWaitsForAnInFlightLaunch(t *testing.T) {
	launching := make(chan struct{})
	finish := make(chan struct{})
	var once sync.Once

	p := New(func(context.Context) (Session, error) {
		once.Do(func() { close(launching) })
		<-finish // still starting Chrome
		return &fakeSession{}, nil
	}, time.Minute)
	defer p.Close()

	go func() {
		l, err := p.Acquire(context.Background())
		if err == nil {
			l.Release()
		}
	}()
	<-launching

	reserved := make(chan struct{})
	go func() {
		res, err := p.Reserve(context.Background())
		if err == nil {
			res.Release()
		}
		close(reserved)
	}()

	select {
	case <-reserved:
		t.Fatal("reserve completed while a browser was still launching")
	case <-time.After(100 * time.Millisecond):
	}

	close(finish)

	select {
	case <-reserved:
	case <-time.After(3 * time.Second):
		t.Fatal("reserve never completed after the launch finished")
	}
}

// Chrome keeps the profile lock until it has actually exited, so a replacement
// must not start while the old one is still shutting down.
func TestReplacementWaitsForTheOldBrowserToExit(t *testing.T) {
	gate := make(chan struct{})
	dying := &blockingSession{closeGate: gate}
	dying.dead.Store(true)

	var opens atomic.Int64
	var openedWhileClosing atomic.Bool

	p := New(func(context.Context) (Session, error) {
		if opens.Add(1) > 1 && !dying.fakeSession.closed.Load() {
			openedWhileClosing.Store(true)
		}
		return &fakeSession{}, nil
	}, time.Minute)
	defer p.Close()

	// Seed the pool with the dying session.
	p.mu.Lock()
	p.session = dying
	p.mu.Unlock()

	got := make(chan struct{})
	go func() {
		l, err := p.Acquire(context.Background())
		if err == nil {
			l.Release()
		}
		close(got)
	}()

	select {
	case <-got:
		t.Fatal("a replacement was acquired before the old browser exited")
	case <-time.After(100 * time.Millisecond):
	}

	close(gate) // the old Chrome finally exits

	select {
	case <-got:
	case <-time.After(3 * time.Second):
		t.Fatal("acquire never completed after the old browser exited")
	}
	if openedWhileClosing.Load() {
		t.Fatal("a replacement browser started while the old one still held the profile")
	}
}

// With warming disabled the browser is retired on every release, and the next
// read must still wait for that shutdown rather than racing it.
func TestZeroIdleBackToBackAcquiresDoNotOverlap(t *testing.T) {
	var live, peak atomic.Int64

	p := New(func(context.Context) (Session, error) {
		n := live.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		return &countedSession{live: &live}, nil
	}, 0)
	defer p.Close()

	for i := 0; i < 5; i++ {
		l, err := p.Acquire(t.Context())
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		l.Release()
	}

	if got := peak.Load(); got > 1 {
		t.Fatalf("%d browsers held the profile at once with warming disabled", got)
	}
}

// countedSession decrements the live counter only once Close has finished, the
// way Chrome releases the profile only once it has exited.
type countedSession struct {
	fakeSession
	live *atomic.Int64
}

func (c *countedSession) Close() error {
	time.Sleep(10 * time.Millisecond) // shutting down
	c.live.Add(-1)
	return c.fakeSession.Close()
}

// A launch outlives the caller that started it. Dropping the opening claim when
// that caller times out would let the next acquire or reservation go at the same
// profile while the first Chrome was still coming up.
func TestTimedOutLaunchKeepsTheOpeningClaim(t *testing.T) {
	launching := make(chan struct{})
	finish := make(chan struct{})
	var launches atomic.Int64

	p := New(func(context.Context) (Session, error) {
		if launches.Add(1) == 1 {
			close(launching)
			<-finish
		}
		return &fakeSession{}, nil
	}, time.Minute)
	defer p.Close()

	// First caller gives up while Chrome is still starting.
	impatient, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(impatient); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected the impatient caller to time out, got %v", err)
	}
	<-launching

	// A second caller must wait for that launch, not start its own.
	second := make(chan struct{})
	go func() {
		l, err := p.Acquire(context.Background())
		if err == nil {
			l.Release()
		}
		close(second)
	}()

	select {
	case <-second:
		t.Fatal("a second acquire proceeded while the first launch was in flight")
	case <-time.After(80 * time.Millisecond):
	}
	if got := launches.Load(); got != 1 {
		t.Fatalf("a second browser was launched behind the first, got %d launches", got)
	}

	close(finish)
	select {
	case <-second:
	case <-time.After(3 * time.Second):
		t.Fatal("the second acquire never completed")
	}
}

// unconfirmedSession stands in for a Chrome that outlived its shutdown and may
// still hold the profile.
type unconfirmedSession struct{ fakeSession }

func (u *unconfirmedSession) Close() error {
	u.fakeSession.closed.Store(true)
	return errors.New("chrome did not exit")
}

// A reservation is a promise that nothing else holds the profile. If a shutdown
// could not confirm the browser exited, that promise cannot be made: a login or
// write would start against a directory some surviving Chrome still owns.
func TestReserveRefusesWhenReleaseIsUnconfirmed(t *testing.T) {
	p := New(func(context.Context) (Session, error) {
		return &unconfirmedSession{}, nil
	}, time.Minute)
	defer p.Close()

	l, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.Release()

	if _, err := p.Reserve(t.Context()); err == nil {
		t.Fatal("a reservation was granted although the browser was never confirmed gone")
	}

	// And the refusal must not leave the profile permanently blocked.
	blocked := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _ = p.Reserve(ctx)
		close(blocked)
	}()
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("a refused reservation left exclusivity asserted")
	}
}

// wedgedSession is running but unresponsive: Alive never answers on its own.
type wedgedSession struct {
	fakeSession
	probing chan struct{}
}

func (w *wedgedSession) Alive(ctx context.Context) bool {
	select {
	case w.probing <- struct{}{}:
	default:
	}
	<-ctx.Done() // never answers; only the probe's deadline ends this
	return false
}

// The liveness probe is browser I/O. Running it under the pool mutex would let a
// wedged connection stall every read, write, login and shutdown behind the same
// lock, so a reservation must stay reachable while a probe is stuck.
func TestWedgedLivenessProbeDoesNotBlockThePool(t *testing.T) {
	wedged := &wedgedSession{probing: make(chan struct{}, 1)}
	var launches atomic.Int64

	p := New(func(context.Context) (Session, error) {
		if launches.Add(1) == 1 {
			return wedged, nil
		}
		return &fakeSession{}, nil
	}, time.Minute)
	defer p.Close()

	// Warm the wedged session into the pool.
	p.mu.Lock()
	p.session = wedged
	p.mu.Unlock()

	go func() {
		l, err := p.Acquire(context.Background())
		if err == nil {
			l.Release()
		}
	}()
	<-wedged.probing // a probe is now stuck

	// Reserve must actually succeed, not merely return. A probe that never
	// answers would keep its lease forever and the reservation would time out --
	// which is why this asserts the error, not just that the call finished.
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := p.Reserve(ctx)
		if res != nil {
			res.Release()
		}
		result <- err
	}()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("a wedged liveness probe blocked the pool: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reserve never returned while a liveness probe was wedged")
	}
}

// flakySession answers the first probe slowly (timing out) and later ones
// quickly, so two concurrent probes disagree about whether it is usable.
type flakySession struct {
	fakeSession
	probes atomic.Int64
	inUse  atomic.Int64
}

func (f *flakySession) Alive(ctx context.Context) bool {
	if f.probes.Add(1) == 1 {
		<-ctx.Done() // first probe never answers
		return false
	}
	return true
}

func (f *flakySession) Close() error {
	if f.inUse.Load() > 0 {
		panic("closed while a caller still held a lease on it")
	}
	return f.fakeSession.Close()
}

// Concurrent probes can disagree under a one-second timeout: one retires the
// browser while another calls it healthy. The retirement has to win, and the
// browser must not be closed while any caller still holds it -- otherwise one
// bad probe turns into several failed requests against a closing connection.
func TestConcurrentProbesDoNotCloseALeasedSession(t *testing.T) {
	flaky := &flakySession{}

	p := New(func(context.Context) (Session, error) {
		return &fakeSession{}, nil
	}, time.Minute)
	defer p.Close()

	p.mu.Lock()
	p.session = flaky
	p.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := p.Acquire(context.Background())
			if err != nil {
				return
			}
			// Whatever we were handed must stay usable for as long as we hold it.
			if s, ok := l.Session.(*flakySession); ok {
				// Held deliberately longer than the failing probe takes to time
				// out, so retirement lands while this caller is mid-read -- the
				// overlap that closing immediately would break.
				s.inUse.Add(1)
				time.Sleep(2 * time.Second)
				s.inUse.Add(-1)
			}
			l.Release()
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("concurrent probes deadlocked the pool")
	}
}

// Concurrent probes can all fail at once. Retirement and shutdown are separate
// steps with one owner precisely so that produces a single Close: tearing the
// same browser down several times races its own cleanup and loses the record of
// whether it actually exited.
func TestConcurrentFailedProbesCloseTheSessionOnce(t *testing.T) {
	var closes atomic.Int64
	doomed := &countingCloseSession{closes: &closes}
	doomed.dead.Store(true)

	p := New(func(context.Context) (Session, error) {
		s := &fakeSession{}
		return s, nil
	}, time.Minute)
	defer p.Close()

	p.mu.Lock()
	p.session = doomed
	p.mu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l, err := p.Acquire(context.Background()); err == nil {
				l.Release()
			}
		}()
	}
	wg.Wait()

	// Give any stray shutdown goroutine a chance to double up.
	time.Sleep(200 * time.Millisecond)

	if got := closes.Load(); got != 1 {
		t.Fatalf("the retired session was closed %d times; exactly one shutdown may run", got)
	}
}

// countingCloseSession records how many times shutdown was started.
type countingCloseSession struct {
	fakeSession
	closes *atomic.Int64
}

func (c *countingCloseSession) Close() error {
	c.closes.Add(1)
	time.Sleep(20 * time.Millisecond) // shutdown takes a moment
	return c.fakeSession.Close()
}

// A shutdown that could not be confirmed is reported to the caller who asked
// for the profile -- but it must not become a permanent state. Chrome exits
// eventually, and a pool that remembered the failure would refuse every
// reservation from then on, leaving sign-in and writes unavailable until a
// restart.
func TestAnUnconfirmedShutdownIsNotRememberedForever(t *testing.T) {
	wedged := errors.New("chrome did not exit; it may still hold the profile")
	var refuse atomic.Bool
	refuse.Store(true)

	p := New(func(context.Context) (Session, error) {
		return &stubborn{refuse: &refuse, err: wedged}, nil
	}, time.Minute)
	defer p.Close()

	lease, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lease.Release()

	if _, err := p.Reserve(t.Context()); !errors.Is(err, wedged) {
		t.Fatalf("first reserve: got %v, want the unconfirmed shutdown", err)
	}

	// The browser has since gone. Nothing about the earlier failure may outlive
	// it: whether the profile is free is a question about now.
	refuse.Store(false)

	lease, err = p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire after the browser exited: %v", err)
	}
	lease.Release()

	res, err := p.Reserve(t.Context())
	if err != nil {
		t.Fatalf("a later reservation must not inherit the old failure: %v", err)
	}
	res.Release()
}

// stubborn is a session whose shutdown does not confirm until refuse is cleared.
type stubborn struct {
	refuse *atomic.Bool
	err    error
}

func (s *stubborn) Close() error {
	if s.refuse.Load() {
		return s.err
	}
	return nil
}

func (s *stubborn) Alive(context.Context) bool { return true }

// A reservation waits for the pool to go quiet so it can shut the session down
// itself and confirm the profile is free. The idle timer must not do it first:
// the reservation would find no session, take the profile, and hand it to a
// login window while that Chrome was still exiting.
//
// The timer is driven by hand here rather than raced against -- the window is a
// few microseconds wide in practice, and a test that waits for it would pass
// almost every time whether or not the guard exists.
func TestTheIdleTimerDoesNotRetireASessionAReservationIsWaitingFor(t *testing.T) {
	open, _, made := counting()
	p := New(open, time.Minute)
	defer p.Close()

	var mu sync.Mutex
	var armed []func()
	p.afterFunc = func(_ time.Duration, fn func()) *time.Timer {
		mu.Lock()
		armed = append(armed, fn)
		mu.Unlock()
		return time.AfterFunc(time.Hour, func() {}) // never fires on its own
	}

	lease, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// Reserve while the lease is still out, so the reservation is waiting for
	// quiet at the moment the last lease is returned.
	reserved := make(chan Reservation, 1)
	failed := make(chan error, 1)
	go func() {
		res, err := p.Reserve(context.Background())
		if err != nil {
			failed <- err
			return
		}
		reserved <- res
	}()

	waitUntil(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.exclusive != nil
	}, "reserve never published exclusivity")

	mu.Lock()
	before := len(armed)
	mu.Unlock()

	lease.Release()

	// Returning the last lease must not arm an idle close: the reservation owns
	// this session's shutdown now.
	mu.Lock()
	after := len(armed)
	mu.Unlock()
	if after != before {
		t.Fatalf("the idle timer was armed while a reservation was waiting (%d new)", after-before)
	}

	select {
	case res := <-reserved:
		defer res.Release()
	case err := <-failed:
		t.Fatalf("reserve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("reserve never completed")
	}

	// Exactly one shutdown, run by the reservation -- not one by the timer and
	// a reservation that believed there was nothing left to close.
	if got := len(*made); got != 1 {
		t.Fatalf("expected one session, got %d", got)
	}
	if !(*made)[0].closed.Load() {
		t.Fatal("the reservation returned without the session being closed")
	}
	p.mu.Lock()
	pending := p.closing
	p.mu.Unlock()
	if pending != nil {
		t.Fatal("a shutdown was still running when the profile was handed over")
	}
}

func waitUntil(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(time.Millisecond)
	}
}

// Shutdown must not leave a Chrome behind. A launch that is still in flight
// ends with a browser holding the profile, and a process that exits without it
// leaves one running that nothing is tracking and nobody expects.
func TestCloseWaitsForAnInFlightLaunch(t *testing.T) {
	landed := make(chan *fakeSession, 1)
	release := make(chan struct{})
	p := New(func(context.Context) (Session, error) {
		<-release
		s := &fakeSession{id: 1}
		landed <- s
		return s, nil
	}, time.Minute)

	go func() { _, _ = p.Acquire(context.Background()) }()
	waitUntil(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.opening != nil
	}, "no launch started")

	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()

	// Close must still be waiting: the browser has not arrived yet.
	select {
	case <-closed:
		t.Fatal("Close returned while a launch was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close never returned")
	}

	session := <-landed
	if !session.closed.Load() {
		t.Fatal("the browser that arrived during shutdown was left running")
	}
}

// ...but not forever. A lease that is never returned must not wedge the server
// open; a browser left running is visible and killable, a process that will not
// exit is neither.
func TestCloseGivesUpOnALeaseThatNeverReturns(t *testing.T) {
	open, _, _ := counting()
	p := New(open, time.Minute)

	lease, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lease.Release()

	// Retire the session while the lease is out, so a retiring browser is
	// waiting on a lease that never comes back.
	p.mu.Lock()
	p.retireLocked(lease.Session)
	p.mu.Unlock()

	original := shutdownWait
	shutdownWait = 100 * time.Millisecond
	defer func() { shutdownWait = original }()

	done := make(chan struct{})
	go func() { p.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung on a lease that was never returned")
	}
}

// A wait that gives up must not leave anything behind waiting. A bounded wait
// whose waiter cannot be woken by the deadline blocks for the life of the
// process, and one per shutdown attempt accumulates.
func TestGivingUpOnAWaitLeavesNoGoroutineBehind(t *testing.T) {
	open, _, _ := counting()
	p := New(open, time.Minute)

	lease, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lease.Release()

	// A retired session with a lease still out: nothing can finish draining.
	p.mu.Lock()
	p.retireLocked(lease.Session)
	p.mu.Unlock()

	original := shutdownWait
	shutdownWait = 20 * time.Millisecond
	defer func() { shutdownWait = original }()

	settle := func() int {
		time.Sleep(150 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	before := settle()

	// Several waits that all have to give up: a shutdown, and reservations that
	// can never be granted.
	for range 5 {
		p.Close()
	}
	for range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, _ = p.Reserve(ctx)
		cancel()
	}

	if after := settle(); after > before+2 {
		t.Fatalf("goroutines grew from %d to %d; waits that gave up are still parked", before, after)
	}
}

// A reservation asked for after the pool has shut down is refused.
//
// This covers the refusal, not the race behind it. Reserve also rechecks in the
// same breath as taking the session, because a shutdown landing between those
// two steps would leave it reporting nothing left to close -- but that window is
// a few instructions wide, and a race that cannot be staged is not a test.
func TestReserveIsRefusedAfterShutdown(t *testing.T) {
	open, _, _ := counting()
	p := New(open, time.Minute)

	lease, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	lease.Release()
	p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := p.Reserve(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("got %v, want ErrClosed", err)
	}
}

// A browser taken out of service still owns the profile until the caller using
// it lets go and it shuts down. Nothing may start a replacement in the meantime:
// the launch would land on a directory the old Chrome still holds, turning one
// caller's retirement into another's failed read.
func TestAcquireWaitsForARetiringSessionToGo(t *testing.T) {
	open, launches, made := counting()
	p := New(open, time.Minute)
	defer p.Close()

	// A holds a lease on the session.
	a, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	// The session is taken out of service while A is still using it, so it
	// cannot be shut down yet.
	p.mu.Lock()
	p.retireLocked(a.Session)
	p.mu.Unlock()

	// C asks for a browser. There is no session, nothing closing and nothing
	// opening -- but the retired one is still out there.
	got := make(chan error, 1)
	go func() {
		lease, err := p.Acquire(context.Background())
		if err == nil {
			lease.Release()
		}
		got <- err
	}()

	select {
	case <-got:
		t.Fatal("a replacement was started while the retired browser still held the profile")
	case <-time.After(200 * time.Millisecond):
	}
	if n := launches.Load(); n != 1 {
		t.Fatalf("%d browsers launched while one was still retiring", n)
	}

	// A lets go; the retired browser shuts down and C may proceed.
	a.Release()

	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("acquire after the retirement finished: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acquire never completed after the retirement finished")
	}

	if !(*made)[0].closed.Load() {
		t.Fatal("the retired browser was never shut down")
	}
}
