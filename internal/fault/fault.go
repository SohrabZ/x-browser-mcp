// Package fault decides what a failure is allowed to say to a caller.
//
// Both transports face the same question and have to answer it the same way:
// which failures are the caller's to see, and which arrive wrapped around
// something that is not -- the path of a profile lock, a filesystem location.
// Keeping that decision here is the point. Three copies of it had already grown,
// they disagreed, and where they disagreed one of them leaked.
package fault

import (
	"context"
	"errors"

	"github.com/SohrabZ/x-browser-mcp/internal/auth"
	"github.com/SohrabZ/x-browser-mcp/internal/browser"
	"github.com/SohrabZ/x-browser-mcp/internal/limit"
	"github.com/SohrabZ/x-browser-mcp/internal/pool"
	"github.com/SohrabZ/x-browser-mcp/internal/read"
	"github.com/SohrabZ/x-browser-mcp/internal/write"
)

// Kind is what sort of failure it was, so a transport can say so in its own
// vocabulary.
type Kind int

const (
	// Internal is a failure the caller can do nothing about, and whose detail is
	// not theirs to see. It is the default precisely because it is the safe one:
	// anything this package has not been taught to recognise is treated as
	// carrying internals until it is.
	Internal Kind = iota

	// Invalid is a request that was wrong. Sending it again will not help.
	Invalid

	// Missing is a post, account or list that X had nothing on.
	Missing

	// Refused is a write the gate turned down.
	Refused

	// LoginRequired is a session that needs signing in.
	LoginRequired

	// Paced is a budget that has been spent.
	Paced

	// Busy is the profile held by a login or a write, or a shutdown underway.
	// Temporary, and worth trying again.
	Busy

	// Timeout is work that did not finish inside the caller's budget.
	Timeout

	// NotApplied is a write that was carried out and that X did not apply. The
	// distinction from Internal matters: the machinery worked, so the caller has
	// a real answer rather than a fault to report.
	NotApplied
)

// internalMessage is all an unrecognised failure may say.
const internalMessage = "internal error"

// Describe reports what kind of failure err was and what may be said about it.
//
// The message is that of the error that was recognised, never the one it arrived
// wrapped in. A profile already in use, for instance, is reported by the sentinel
// that says so and not by the wrapper carrying the path of its lock.
//
// A nil error is Internal with no message; callers are expected to check for
// success before asking.
//
// Two things this does not do. It trusts the message of an error it recognises:
// whoever builds one is responsible for it holding nothing internal, and the
// types exist so that is a deliberate act rather than a wrapper's accident. And
// its precedence is the order below, which matters only for an error satisfying
// two cases at once -- nothing builds one today, and the order says an outcome
// X reported beats a deadline that also passed, because the outcome is the more
// specific answer.
func Describe(err error) (Kind, string) {
	if err == nil {
		return Internal, ""
	}

	var (
		exhausted  *limit.ExhaustedError
		badRead    *read.InvalidError
		missing    *read.NotFoundError
		badWrite   *write.InvalidError
		goneWrite  *write.NotFoundError
		notApplied *write.NotAppliedError
	)

	switch {
	case errors.Is(err, write.ErrDisabled):
		return Refused, write.ErrDisabled.Error()
	case errors.Is(err, write.ErrBadConfirmation):
		return Refused, write.ErrBadConfirmation.Error()
	case errors.Is(err, auth.ErrLoginRequired):
		return LoginRequired, auth.ErrLoginRequired.Error()
	case errors.As(err, &exhausted):
		return Paced, exhausted.Error()
	case errors.As(err, &badRead):
		return Invalid, badRead.Error()
	case errors.As(err, &badWrite):
		return Invalid, badWrite.Error()
	case errors.As(err, &missing):
		return Missing, missing.Error()
	case errors.As(err, &goneWrite):
		return Missing, goneWrite.Error()
	case errors.As(err, &notApplied):
		return NotApplied, notApplied.Error()
	case errors.Is(err, browser.ErrProfileInUse):
		return Busy, browser.ErrProfileInUse.Error()
	case errors.Is(err, pool.ErrClosed):
		return Busy, pool.ErrClosed.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout, "the request did not finish in time"
	default:
		return Internal, internalMessage
	}
}
