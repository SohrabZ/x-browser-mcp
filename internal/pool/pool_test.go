package pool

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSession records whether it was closed.
type fakeSession struct {
	id     int
	closed atomic.Bool
}

func (f *fakeSession) Close() { f.closed.Store(true) }

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

// deadSession reports itself unusable, as a session does once its browser has
// crashed, been killed, or been quit by the user.
type deadSession struct{ fakeSession }

func (d *deadSession) Alive() bool { return false }

// A browser that dies underneath the pool must be replaced, not handed out
// again. Reusing it fails every call, and nothing recovers until the idle timer
// happens to fire.
func TestDeadSessionIsReplaced(t *testing.T) {
	var launches atomic.Int64
	first := &deadSession{}

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
