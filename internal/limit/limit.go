// Package limit paces how often the server is allowed to drive a browser at X.
//
// Fetching too eagerly is what gets a session flagged, so every uncached path
// goes through a budget. Reads and writes each get their own.
package limit

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ExhaustedError reports that the rolling budget is spent.
type ExhaustedError struct {
	RetryAfter time.Duration
}

func (e *ExhaustedError) Error() string {
	return fmt.Sprintf("rate budget exhausted; retry in %s", e.RetryAfter.Round(time.Second))
}

// Budget enforces a floor between calls and a ceiling within a rolling window.
type Budget struct {
	minInterval time.Duration
	window      time.Duration
	max         int

	// now is swappable so tests do not have to sleep.
	now func() time.Time

	mu      sync.Mutex
	last    time.Time
	history []time.Time
}

// New builds a budget allowing at most max calls per window, spaced at least
// minInterval apart.
func New(minInterval, window time.Duration, max int) *Budget {
	return &Budget{
		minInterval: minInterval,
		window:      window,
		max:         max,
		now:         time.Now,
	}
}

// Wait blocks until the caller may proceed, then records the call.
//
// It returns *ExhaustedError when the window is full — that is a real answer
// for the caller to surface, not something to wait out, since the wait could be
// most of the window.
//
// The lock is never held while sleeping: a slow caller would otherwise stall
// every other request behind it for the whole interval.
func (b *Budget) Wait(ctx context.Context) error {
	for {
		wait, err := b.reserve()
		if err != nil {
			return err
		}
		if wait <= 0 {
			return nil
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// reserve either claims a slot (0, nil), asks the caller to sleep (>0, nil), or
// refuses (0, *ExhaustedError).
func (b *Budget) reserve() (time.Duration, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	b.prune(now)

	if b.max > 0 && len(b.history) >= b.max {
		oldest := b.history[0]
		retry := oldest.Add(b.window).Sub(now)
		if retry < 0 {
			retry = 0
		}
		return 0, &ExhaustedError{RetryAfter: retry}
	}

	if !b.last.IsZero() {
		if wait := b.minInterval - now.Sub(b.last); wait > 0 {
			return wait, nil
		}
	}

	b.last = now
	b.history = append(b.history, now)
	return 0, nil
}

// prune drops calls that have aged out of the window.
func (b *Budget) prune(now time.Time) {
	if b.window <= 0 {
		b.history = b.history[:0]
		return
	}
	cutoff := now.Add(-b.window)
	kept := b.history[:0]
	for _, at := range b.history {
		if at.After(cutoff) {
			kept = append(kept, at)
		}
	}
	b.history = kept
}

// Remaining reports how many calls are still available in the current window.
func (b *Budget) Remaining() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.prune(b.now())
	left := b.max - len(b.history)
	if left < 0 {
		return 0
	}
	return left
}
