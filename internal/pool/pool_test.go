package pool

import (
	"context"
	"errors"
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

	p := New(func(context.Context) (Session, error) {
		close(launching)
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
