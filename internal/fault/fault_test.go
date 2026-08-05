package fault

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/auth"
	"github.com/SohrabZ/x-browser-mcp/internal/browser"
	"github.com/SohrabZ/x-browser-mcp/internal/limit"
	"github.com/SohrabZ/x-browser-mcp/internal/pool"
	"github.com/SohrabZ/x-browser-mcp/internal/read"
	"github.com/SohrabZ/x-browser-mcp/internal/write"
)

func TestDescribeClassifiesEachFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		kind Kind
		says string
	}{
		{"writes disabled", write.ErrDisabled, Refused, write.ErrDisabled.Error()},
		{"wrong token", write.ErrBadConfirmation, Refused, write.ErrBadConfirmation.Error()},
		{"not signed in", auth.ErrLoginRequired, LoginRequired, auth.ErrLoginRequired.Error()},
		{"budget spent", &limit.ExhaustedError{}, Paced, (&limit.ExhaustedError{}).Error()},
		{"bad read request", &read.InvalidError{Reason: "list id is required"}, Invalid, "list id is required"},
		{"bad write request", &write.InvalidError{Reason: "post text is required"}, Invalid, "post text is required"},
		{"nothing there", &read.NotFoundError{Reason: "no posts found"}, Missing, "no posts found"},
		{"X did not apply it", &write.NotAppliedError{Reason: "like did not stick"}, NotApplied, "like did not stick"},
		{"no such post to act on", &write.NotFoundError{Reason: "no post at that address"}, Missing, "no post at that address"},
		{"profile in use", browser.ErrProfileInUse, Busy, browser.ErrProfileInUse.Error()},
		{"shutting down", pool.ErrClosed, Busy, pool.ErrClosed.Error()},
		{"out of time", context.DeadlineExceeded, Timeout, "the request did not finish in time"},
	}

	for _, c := range cases {
		kind, says := Describe(c.err)
		if kind != c.kind {
			t.Errorf("%s: kind %v, want %v", c.name, kind, c.kind)
		}
		if says != c.says {
			t.Errorf("%s: says %q, want %q", c.name, says, c.says)
		}
	}
}

// Anything unrecognised is Internal, and Internal says nothing. That is the
// direction the default has to fail in: an error this package has not been taught
// about is assumed to carry internals until it has.
func TestAnUnrecognisedFailureSaysNothing(t *testing.T) {
	for _, err := range []error{
		errors.New("cdp connection closed"),
		fmt.Errorf("decode scraped posts: %w", errors.New("unexpected end of JSON")),
		context.Canceled,
	} {
		kind, says := Describe(err)
		if kind != Internal {
			t.Errorf("%v: kind %v, want Internal", err, kind)
		}
		if says != internalMessage {
			t.Errorf("%v: says %q, want %q", err, says, internalMessage)
		}
	}
}

// The whole point: what is said is the message of the error that was recognised,
// never the one it arrived wrapped in. The wrapper is where the internals are.
func TestDescribeNeverRepeatsTheWrapper(t *testing.T) {
	secret := "/Users/someone/.x-browser-mcp/profile/SingletonLock"

	wrapped := []error{
		fmt.Errorf("%w (%s)", browser.ErrProfileInUse, secret),
		fmt.Errorf("open %s: %w", secret, pool.ErrClosed),
		fmt.Errorf("read %s: %w", secret, &read.NotFoundError{Reason: "no posts found"}),
		fmt.Errorf("lease %s: %w", secret, auth.ErrLoginRequired),
		fmt.Errorf("write to %s: %w", secret, write.ErrBadConfirmation),
		fmt.Errorf("under %s: %w", secret, &write.NotAppliedError{Reason: "like did not stick"}),
		fmt.Errorf("at %s: %w", secret, context.DeadlineExceeded),
		fmt.Errorf("in %s: %w", secret, errors.New("something internal")),
	}

	for _, err := range wrapped {
		kind, says := Describe(err)
		if strings.Contains(says, secret) {
			t.Errorf("%v (%v) leaked the wrapper: %q", err, kind, says)
		}
	}
}

// A sentinel still has to be found through a wrapper, or classifying it is
// pointless -- the reader returns these from under its own context.
func TestDescribeSeesThroughAWrapper(t *testing.T) {
	kind, _ := Describe(fmt.Errorf("read list: %w", &read.NotFoundError{Reason: "no posts found"}))
	if kind != Missing {
		t.Errorf("kind %v, want Missing through a wrapper", kind)
	}

	kind, _ = Describe(fmt.Errorf("open browser: %w", browser.ErrProfileInUse))
	if kind != Busy {
		t.Errorf("kind %v, want Busy through a wrapper", kind)
	}
}

// A deadline that has already passed arrives as the wrapped context error from
// anything ctx-aware, so that path has to classify too.
func TestARealDeadlineIsATimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()

	if kind, _ := Describe(ctx.Err()); kind != Timeout {
		t.Errorf("kind %v, want Timeout", kind)
	}
}

func TestNoErrorIsNotDescribed(t *testing.T) {
	kind, says := Describe(nil)
	if kind != Internal || says != "" {
		t.Errorf("got %v %q, want Internal and no message", kind, says)
	}
}

// Precedence only matters for an error satisfying two cases at once. Nothing
// builds one today, so this pins the policy rather than a behaviour: an outcome X
// reported beats a deadline that also passed, because the outcome is the more
// specific answer. If that is ever wrong, it should be wrong loudly here.
func TestPrecedenceBetweenTwoCauses(t *testing.T) {
	cases := []struct {
		name string
		err  error
		kind Kind
	}{
		{"outcome beats deadline",
			errors.Join(&write.NotAppliedError{Reason: "like did not stick"}, context.DeadlineExceeded), NotApplied},
		{"absence beats deadline",
			errors.Join(&read.NotFoundError{Reason: "no posts found"}, context.DeadlineExceeded), Missing},
		{"a refusal beats everything, since nothing was attempted",
			errors.Join(write.ErrBadConfirmation, context.DeadlineExceeded), Refused},
		{"a deadline still wins over an unrecognised fault",
			errors.Join(errors.New("cdp closed"), context.DeadlineExceeded), Timeout},
	}

	for _, c := range cases {
		if kind, _ := Describe(c.err); kind != c.kind {
			t.Errorf("%s: kind %v, want %v", c.name, kind, c.kind)
		}
	}
}
