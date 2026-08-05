package auth

import (
	"context"
	"errors"
	"github.com/SohrabZ/x-browser-mcp/internal/pool"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeClock lets tests move time without sleeping.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *fakeClock {
	return &fakeClock{at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// The probe path needs a real browser, so these tests cover the parts that do
// not: caching, login-in-progress short-circuiting, and invalidation.

func TestStatusShortCircuitsWhileLoginIsOpen(t *testing.T) {
	clock := newClock()
	m := New(Options{ProfileDir: "/tmp/profile", LoginTimeout: 4 * time.Minute})
	m.now = clock.now

	m.login = &attempt{deadline: clock.now().Add(time.Minute)}

	status, ok := m.loginInProgress()
	if !ok {
		t.Fatal("expected an in-progress login to short-circuit")
	}
	if status.State != StateInProgress {
		t.Fatalf("got state %q, want %q", status.State, StateInProgress)
	}
	if status.LoggedIn {
		t.Error("an in-progress login is not a signed-in session")
	}
}

// A login that overran its timeout is still a login. The window is open and
// still owns the profile, so reporting otherwise would send reads at a
// directory Chrome is holding, and they would fail on the profile lock.
func TestOverrunningLoginStillOwnsTheProfile(t *testing.T) {
	clock := newClock()
	m := New(Options{ProfileDir: "/tmp/profile"})
	m.now = clock.now
	m.login = &attempt{deadline: clock.now().Add(time.Minute)}

	clock.advance(10 * time.Minute) // well past the deadline

	status, ok := m.loginInProgress()
	if !ok {
		t.Fatal("an open login window must keep short-circuiting past its deadline")
	}
	if status.State != StateInProgress {
		t.Fatalf("got state %q, want %q", status.State, StateInProgress)
	}
}

// fakeReservation records whether the profile was handed back.
type fakeReservation struct{ released atomic.Bool }

func (f *fakeReservation) Release() { f.released.Store(true) }

// The deadline says nothing about whether Chrome exited. Releasing on the timer
// would give the profile to a read while the login window still held it.
func TestWatchKeepsTheProfileUntilTheWindowExits(t *testing.T) {
	m := New(Options{LoginTimeout: time.Millisecond})

	done := make(chan error, 1)
	res := &fakeReservation{}
	att := &attempt{
		done:     done,
		deadline: time.Now().Add(5 * time.Millisecond), // fires almost immediately
		profile:  res,
	}
	m.login = att

	go m.watch(att)

	// Well past the deadline, with the process still running.
	time.Sleep(120 * time.Millisecond)
	if res.released.Load() {
		t.Fatal("the profile was released while the login window was still open")
	}
	m.mu.Lock()
	stillOwned := m.login == att
	m.mu.Unlock()
	if !stillOwned {
		t.Fatal("the login marker was cleared while the window was still open")
	}

	// The window closes.
	done <- nil

	deadline := time.After(2 * time.Second)
	for !res.released.Load() {
		select {
		case <-deadline:
			t.Fatal("the profile was never released after the window exited")
		case <-time.After(5 * time.Millisecond):
		}
	}
	m.mu.Lock()
	cleared := m.login == nil
	m.mu.Unlock()
	if !cleared {
		t.Error("the login marker should be cleared once the window exits")
	}
}

func TestPositiveVerdictIsCachedForTheFullTTL(t *testing.T) {
	clock := newClock()
	m := New(Options{StatusTTL: 5 * time.Minute})
	m.now = clock.now

	m.remember(Status{State: StateReady, LoggedIn: true})

	clock.advance(4 * time.Minute)
	if _, ok := m.cachedStatus(); !ok {
		t.Fatal("a signed-in verdict should still be cached")
	}

	clock.advance(2 * time.Minute)
	if _, ok := m.cachedStatus(); ok {
		t.Fatal("the cache should have expired")
	}
}

// A negative verdict expires quickly so a login that just completed is picked
// up without waiting out the full positive TTL.
func TestNegativeVerdictExpiresQuickly(t *testing.T) {
	clock := newClock()
	m := New(Options{StatusTTL: time.Hour})
	m.now = clock.now

	m.remember(Status{State: StateRequired, LoggedIn: false})

	clock.advance(negativeTTL + time.Second)
	if _, ok := m.cachedStatus(); ok {
		t.Fatal("a not-signed-in verdict must not be cached for the positive TTL")
	}
}

func TestInvalidateDropsTheCache(t *testing.T) {
	clock := newClock()
	m := New(Options{StatusTTL: time.Hour})
	m.now = clock.now
	m.remember(Status{State: StateReady, LoggedIn: true})

	m.Invalidate()

	if _, ok := m.cachedStatus(); ok {
		t.Fatal("expected the cached verdict to be gone")
	}
}

func TestStartLoginReturnsTheExistingDeadlineWhenOneIsOpen(t *testing.T) {
	clock := newClock()
	launches := 0
	m := New(Options{
		LoginTimeout: 4 * time.Minute,
		LaunchLogin: func() (*exec.Cmd, error) {
			launches++
			return exec.Command("true"), nil
		},
	})
	m.now = clock.now

	existing := clock.now().Add(2 * time.Minute)
	m.login = &attempt{deadline: existing}

	got, err := m.StartLogin(t.Context())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !got.Equal(existing) {
		t.Fatalf("got deadline %s, want the existing %s", got, existing)
	}
	if launches != 0 {
		t.Fatal("must not open a second browser onto the same profile")
	}
}

func TestStartLoginReportsLauncherFailure(t *testing.T) {
	m := New(Options{
		LoginTimeout: time.Minute,
		LaunchLogin: func() (*exec.Cmd, error) {
			return nil, errors.New("chrome missing")
		},
	})

	if _, err := m.StartLogin(t.Context()); err == nil {
		t.Fatal("expected the launch failure to surface")
	}
}

// The watcher must clear the marker as soon as the browser exits; otherwise
// status keeps claiming a login is in progress for the rest of the timeout.
func TestWatchClearsTheMarkerWhenTheBrowserExits(t *testing.T) {
	m := New(Options{LoginTimeout: time.Hour})

	done := make(chan error, 1)
	att := &attempt{done: done, deadline: time.Now().Add(time.Hour)}
	m.login = att

	go m.watch(att)
	done <- nil

	deadline := time.After(2 * time.Second)
	for {
		m.mu.Lock()
		cleared := m.login == nil
		m.mu.Unlock()
		if cleared {
			return
		}
		select {
		case <-deadline:
			t.Fatal("watcher did not clear the in-progress marker")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestRequireSurfacesLoginRequired(t *testing.T) {
	clock := newClock()
	m := New(Options{StatusTTL: time.Hour})
	m.now = clock.now
	m.remember(Status{State: StateRequired, LoggedIn: false})

	if err := m.Require(t.Context()); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("got %v, want ErrLoginRequired", err)
	}
}

func TestRequirePassesWhenSignedIn(t *testing.T) {
	clock := newClock()
	m := New(Options{StatusTTL: time.Hour})
	m.now = clock.now
	m.remember(Status{State: StateReady, LoggedIn: true})

	if err := m.Require(t.Context()); err != nil {
		t.Fatalf("expected a signed-in session to pass, got %v", err)
	}
}

// An abandoned login window must not block reads and writes forever. The
// deadline is a bound on how long the profile may be unavailable, so it is
// enforced by closing the window -- but ownership is still only released once
// the process has actually exited.
func TestAbandonedLoginIsClosedAtTheDeadline(t *testing.T) {
	// A real process that would otherwise outlive the deadline by minutes.
	cmd := exec.Command("sleep", "120")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	m := New(Options{LoginTimeout: 50 * time.Millisecond})
	res := &fakeReservation{}
	att := &attempt{
		cmd:      cmd,
		done:     waitFor(cmd),
		deadline: time.Now().Add(50 * time.Millisecond),
		profile:  res,
	}
	m.login = att

	done := make(chan struct{})
	go func() { m.watch(att); close(done) }()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("an abandoned login blocked the profile past its deadline")
	}

	if !res.released.Load() {
		t.Error("the profile should be released once the window is gone")
	}
	m.mu.Lock()
	cleared := m.login == nil
	m.mu.Unlock()
	if !cleared {
		t.Error("the login marker should be cleared")
	}

	// The process really exited rather than being merely forgotten.
	if err := cmd.Process.Signal(syscall.Signal(0)); err == nil {
		t.Error("the login process is still running after the deadline")
	}
}

// slowReserver takes as long as a real reservation does -- long enough for a
// second caller to arrive while the first is still getting hold of the profile.
type slowReserver struct {
	delay time.Duration
	held  atomic.Int64
}

func (s *slowReserver) Reserve(ctx context.Context) (pool.Reservation, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.held.Add(1)
	return &fakeReservation{}, nil
}

// A second start_login that arrives while the first is still reserving must get
// the first window's deadline, not a window of its own. Reserving is slow, and
// a caller that queued behind it used to wake up after the first login had
// finished, see nothing in progress, and open a browser nobody asked for --
// holding the profile for another full timeout.
func TestASecondStartLoginDuringReservationJoinsTheFirst(t *testing.T) {
	var launches atomic.Int64
	reserver := &slowReserver{delay: 150 * time.Millisecond}

	m := New(Options{
		LoginTimeout: 4 * time.Minute,
		Reserve:      reserver,
		LaunchLogin: func() (*exec.Cmd, error) {
			launches.Add(1)
			cmd := exec.Command("sleep", "30")
			if err := cmd.Start(); err != nil {
				return nil, err
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })
			return cmd, nil
		},
	})

	var wg sync.WaitGroup
	deadlines := make([]time.Time, 2)
	errs := make([]error, 2)
	for i := range deadlines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			deadlines[i], errs[i] = m.StartLogin(context.Background())
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := launches.Load(); got != 1 {
		t.Fatalf("opened %d login windows; only one may hold the profile", got)
	}
	if !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("callers got different deadlines: %s and %s", deadlines[0], deadlines[1])
	}
	if got := reserver.held.Load(); got != 1 {
		t.Fatalf("took the profile %d times, want 1", got)
	}
}

// A login that never gets off the ground must not leave status claiming one is
// in progress forever.
func TestAFailedStartLoginReleasesTheClaim(t *testing.T) {
	m := New(Options{
		LoginTimeout: time.Minute,
		LaunchLogin:  func() (*exec.Cmd, error) { return nil, errors.New("chrome missing") },
	})

	if _, err := m.StartLogin(context.Background()); err == nil {
		t.Fatal("expected the launch failure to surface")
	}
	if _, inProgress := m.loginInProgress(); inProgress {
		t.Fatal("a login that never opened is not in progress")
	}
}
