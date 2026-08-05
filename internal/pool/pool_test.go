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

func TestConcurrentAcquiresShareOneBrowser(t *testing.T) {
	open, launches, made := counting()
	p := New(open, time.Minute)
	defer p.Close()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := p.Acquire(t.Context())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			time.Sleep(5 * time.Millisecond)
			l.Release()
		}()
	}
	wg.Wait()

	// Racing callers may each open one, but only one may be kept: two browsers
	// on a single profile is exactly what the profile lock forbids.
	if !p.Warm() {
		t.Fatal("a session should still be warm")
	}
	kept := 0
	for _, s := range *made {
		if !s.closed.Load() {
			kept++
		}
	}
	if kept != 1 {
		t.Fatalf("exactly one session may be retained, got %d (of %d launched)", kept, launches.Load())
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
func TestEvictClosesTheSession(t *testing.T) {
	open, _, made := counting()
	p := New(open, time.Minute)
	defer p.Close()

	l, _ := p.Acquire(t.Context())
	l.Release()

	if err := p.Evict(t.Context()); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if p.Warm() {
		t.Fatal("the pool should hold nothing after eviction")
	}
	if !(*made)[0].closed.Load() {
		t.Fatal("the evicted session should have been closed")
	}
}

// Eviction must not yank a browser out from under an in-flight read.
func TestEvictWaitsForOutstandingLeases(t *testing.T) {
	open, _, _ := counting()
	p := New(open, time.Minute)
	defer p.Close()

	l, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}

	evicted := make(chan error, 1)
	go func() { evicted <- p.Evict(context.Background()) }()

	select {
	case <-evicted:
		t.Fatal("evict returned while a lease was still held")
	case <-time.After(50 * time.Millisecond):
	}

	l.Release()

	select {
	case err := <-evicted:
		if err != nil {
			t.Fatalf("evict: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("evict did not finish after the lease was released")
	}
}

// A read that hangs must not block eviction forever; the caller's context bounds it.
func TestEvictHonoursItsContext(t *testing.T) {
	open, _, _ := counting()
	p := New(open, time.Minute)
	defer p.Close()

	l, _ := p.Acquire(t.Context()) // deliberately never released
	defer l.Release()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if err := p.Evict(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want DeadlineExceeded", err)
	}
}

func TestAcquireAfterEvictOpensAgain(t *testing.T) {
	open, launches, _ := counting()
	p := New(open, time.Minute)
	defer p.Close()

	l, _ := p.Acquire(t.Context())
	l.Release()
	_ = p.Evict(t.Context())

	l2, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatalf("acquire after evict: %v", err)
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
