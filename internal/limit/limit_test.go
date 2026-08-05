package limit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// clock lets the tests advance time explicitly instead of sleeping.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock {
	return &clock{at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

func budget(c *clock, minInterval, window time.Duration, max int) *Budget {
	b := New(Params{MinInterval: minInterval, Window: window, Max: max})
	b.now = c.now
	// Deterministic: jitter is exercised by its own tests.
	b.jitterFor = func(time.Duration) time.Duration { return 0 }
	return b
}

func TestFirstCallIsImmediate(t *testing.T) {
	c := newClock()
	b := budget(c, time.Minute, time.Hour, 5)

	if err := b.Wait(t.Context()); err != nil {
		t.Fatalf("first call should not be paced: %v", err)
	}
}

func TestBudgetExhaustsAndReportsRetryAfter(t *testing.T) {
	c := newClock()
	b := budget(c, 0, time.Hour, 2)

	for i := 0; i < 2; i++ {
		if err := b.Wait(t.Context()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	err := b.Wait(t.Context())
	var exhausted *ExhaustedError
	if !errors.As(err, &exhausted) {
		t.Fatalf("expected ExhaustedError, got %v", err)
	}
	if exhausted.RetryAfter != time.Hour {
		t.Fatalf("retry after should be the full window from the oldest call, got %s", exhausted.RetryAfter)
	}
}

func TestCallsAgeOutOfTheWindow(t *testing.T) {
	c := newClock()
	b := budget(c, 0, time.Hour, 2)

	_ = b.Wait(t.Context())
	_ = b.Wait(t.Context())
	if err := b.Wait(t.Context()); err == nil {
		t.Fatal("expected the budget to be spent")
	}

	c.advance(time.Hour + time.Second)

	if err := b.Wait(t.Context()); err != nil {
		t.Fatalf("budget should refill once the window passes: %v", err)
	}
}

func TestRemainingTracksUsage(t *testing.T) {
	c := newClock()
	b := budget(c, 0, time.Hour, 3)

	if got := b.Remaining(); got != 3 {
		t.Fatalf("got %d, want 3", got)
	}
	_ = b.Wait(t.Context())
	if got := b.Remaining(); got != 2 {
		t.Fatalf("got %d, want 2", got)
	}

	c.advance(2 * time.Hour)
	if got := b.Remaining(); got != 3 {
		t.Fatalf("window passed, got %d, want 3", got)
	}
}

func TestMinIntervalPacesConsecutiveCalls(t *testing.T) {
	c := newClock()
	// A real interval, but the clock never advances on its own, so a paced call
	// would block forever -- the context is what proves pacing was applied.
	b := budget(c, time.Minute, time.Hour, 10)

	if err := b.Wait(t.Context()); err != nil {
		t.Fatalf("first call: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	if err := b.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second call should have been paced until the context expired, got %v", err)
	}
}

func TestPacedCallProceedsOnceIntervalPasses(t *testing.T) {
	c := newClock()
	b := budget(c, 50*time.Millisecond, time.Hour, 10)

	if err := b.Wait(t.Context()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	// Advance past the interval so the retry loop finds a free slot.
	c.advance(time.Second)

	if err := b.Wait(t.Context()); err != nil {
		t.Fatalf("second call should proceed: %v", err)
	}
}

func TestCancelledContextIsReported(t *testing.T) {
	c := newClock()
	b := budget(c, time.Hour, 24*time.Hour, 10)
	_ = b.Wait(t.Context())

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := b.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
}

// The lock must not be held while sleeping, or one paced caller stalls every
// other request for the whole interval.
func TestConcurrentCallersDoNotBlockOnALockedBudget(t *testing.T) {
	c := newClock()
	b := budget(c, time.Hour, 24*time.Hour, 10)
	_ = b.Wait(t.Context())

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.Wait(ctx)
		}()
	}
	go func() { wg.Wait(); close(done) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("callers serialised behind a held lock instead of waiting independently")
	}
}

// Engagement arriving at a fixed cadence is a signature no human produces, so
// the gap between writes is spread over a range rather than being constant.
func TestJitterExtendsTheGapBetweenCalls(t *testing.T) {
	c := newClock()
	b := New(Params{MinInterval: time.Minute, Jitter: time.Minute, Window: time.Hour, Max: 10})
	b.now = c.now
	b.jitterFor = func(max time.Duration) time.Duration { return max } // worst case

	if err := b.Wait(t.Context()); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// One minute in, the base interval has passed but the jitter has not.
	c.advance(90 * time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if err := b.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("jitter should still be holding the call, got %v", err)
	}

	// Past MinInterval+Jitter it proceeds.
	c.advance(time.Minute)
	if err := b.Wait(t.Context()); err != nil {
		t.Fatalf("call should proceed once the jittered gap passes: %v", err)
	}
}

func TestRandomJitterStaysInRange(t *testing.T) {
	for i := 0; i < 200; i++ {
		got := randomJitter(time.Minute)
		if got < 0 || got >= time.Minute {
			t.Fatalf("jitter %s outside [0, 1m)", got)
		}
	}
	if got := randomJitter(0); got != 0 {
		t.Fatalf("zero jitter must stay zero, got %s", got)
	}
}
