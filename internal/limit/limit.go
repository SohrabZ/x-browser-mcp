// Package limit paces how often the server is allowed to drive a browser at X.
//
// Fetching too eagerly is what gets a session flagged, so every uncached path
// goes through a budget. Reads and writes each get their own.
package limit

import (
	"context"
	"fmt"
	"math/rand"
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

// Params configures a Budget.
type Params struct {
	MinInterval time.Duration
	Window      time.Duration
	Max         int

	// Jitter spreads the gap between calls over MinInterval..MinInterval+Jitter.
	//
	// A fixed interval is itself a signature: nothing human acts at exactly the
	// same spacing every time. It matters for actions X can see as engagement,
	// and not at all for reads.
	Jitter time.Duration
}

// Budget enforces a floor between calls and a ceiling within a rolling window.
type Budget struct {
	params Params

	// now and jitterFor are swappable so tests need not sleep or flake.
	now       func() time.Time
	jitterFor func(time.Duration) time.Duration

	mu      sync.Mutex
	last    time.Time
	history []time.Time
}

// New builds a budget from p.
func New(p Params) *Budget {
	return &Budget{
		params:    p,
		now:       time.Now,
		jitterFor: randomJitter,
	}
}

func randomJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
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

	if b.params.Max > 0 && len(b.history) >= b.params.Max {
		oldest := b.history[0]
		retry := oldest.Add(b.params.Window).Sub(now)
		if retry < 0 {
			retry = 0
		}
		return 0, &ExhaustedError{RetryAfter: retry}
	}

	if !b.last.IsZero() {
		gap := b.params.MinInterval + b.jitterFor(b.params.Jitter)
		if wait := gap - now.Sub(b.last); wait > 0 {
			return wait, nil
		}
	}

	b.last = now
	b.history = append(b.history, now)
	return 0, nil
}

// prune drops calls that have aged out of the window.
func (b *Budget) prune(now time.Time) {
	if b.params.Window <= 0 {
		b.history = b.history[:0]
		return
	}
	cutoff := now.Add(-b.params.Window)
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
	left := b.params.Max - len(b.history)
	if left < 0 {
		return 0
	}
	return left
}
