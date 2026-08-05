package auth

import (
	"errors"
	"os/exec"
	"sync"
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

func TestExpiredLoginNoLongerShortCircuits(t *testing.T) {
	clock := newClock()
	m := New(Options{ProfileDir: "/tmp/profile"})
	m.now = clock.now
	m.login = &attempt{deadline: clock.now().Add(time.Minute)}

	clock.advance(2 * time.Minute)

	if _, ok := m.loginInProgress(); ok {
		t.Fatal("an expired login attempt must stop short-circuiting")
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
