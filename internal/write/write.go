// Package write implements the mutating X actions, behind explicit gating.
//
// Nothing here is destructive: there is no delete, unfollow, block or DM, so
// the worst outcome of a mistake is something the user can undo by hand.
package write

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod/lib/proto"

	"github.com/SohrabZ/x-browser-mcp/internal/auth"
	"github.com/SohrabZ/x-browser-mcp/internal/browser"
	"github.com/SohrabZ/x-browser-mcp/internal/limit"
	"github.com/SohrabZ/x-browser-mcp/internal/pool"
	"github.com/SohrabZ/x-browser-mcp/internal/xui"
)

// MaxPostRunes is X's limit for a standard account.
const MaxPostRunes = 280

// Action names, used in the audit log and error messages.
const (
	ActionPost       = "post"
	ActionReply      = "reply"
	ActionLike       = "like"
	ActionRepost     = "repost"
	ActionBookmark   = "bookmark"
	ActionUnbookmark = "unbookmark"
)

// Opener starts a browser session against the persistent profile.
type Opener func(ctx context.Context, headless bool) (*browser.Session, error)

// Reserver takes exclusive use of the profile.
//
// A write runs in its own visible browser, and only one Chrome may hold a
// user-data-dir, so the reservation is held for the whole action rather than
// released once the browser has started.
type Reserver interface {
	Reserve(ctx context.Context) (pool.Reservation, error)
}

// Writer performs mutating actions.
type Writer struct {
	open    Opener
	auth    *auth.Manager
	gate    *Gate
	budget  *limit.Budget
	audit   *Auditor
	reserve Reserver
	timeout time.Duration

	// onChange lets the reader drop cached results after a successful write.
	onChange func()
}

// Options configures a Writer.
type Options struct {
	Open     Opener
	Auth     *auth.Manager
	Gate     *Gate
	Budget   *limit.Budget
	Audit    *Auditor
	Reserve  Reserver
	Timeout  time.Duration
	OnChange func()
}

// New builds a Writer.
func New(opts Options) *Writer {
	return &Writer{
		open:     opts.Open,
		auth:     opts.Auth,
		gate:     opts.Gate,
		budget:   opts.Budget,
		audit:    opts.Audit,
		reserve:  opts.Reserve,
		timeout:  opts.Timeout,
		onChange: opts.OnChange,
	}
}

// Enabled reports whether writes are available.
func (w *Writer) Enabled() bool { return w.gate.Enabled() }

// Post publishes a new post.
func (w *Writer) Post(ctx context.Context, text, confirm string) error {
	if err := ValidateText(text); err != nil {
		return err
	}
	return w.do(ctx, Record{Action: ActionPost, Excerpt: text}, confirm, func(p *browser.Page) error {
		if err := p.Goto(xui.HomeURL); err != nil {
			return err
		}
		return compose(p, text)
	})
}

// Reply posts a response to an existing post.
func (w *Writer) Reply(ctx context.Context, handle, postID, text, confirm string) error {
	if err := ValidateText(text); err != nil {
		return err
	}
	target, err := postTarget(handle, postID)
	if err != nil {
		return err
	}
	return w.do(ctx, Record{Action: ActionReply, Target: target, Excerpt: text}, confirm, func(p *browser.Page) error {
		if err := p.Goto(target); err != nil {
			return err
		}
		return compose(p, text)
	})
}

// Like likes a post.
func (w *Writer) Like(ctx context.Context, handle, postID, confirm string) error {
	return w.tap(ctx, ActionLike, handle, postID, confirm, xui.SelLikeButton, xui.SelUnlikeButton)
}

// Repost reposts a post. X asks for confirmation in a menu, so the confirm item
// is clicked when it appears.
func (w *Writer) Repost(ctx context.Context, handle, postID, confirm string) error {
	target, err := postTarget(handle, postID)
	if err != nil {
		return err
	}
	return w.do(ctx, Record{Action: ActionRepost, Target: target}, confirm, func(p *browser.Page) error {
		if err := p.Goto(target); err != nil {
			return err
		}
		if hasOnPost(p, postID, xui.SelUnrepostButton, appliedWait) {
			return nil // already reposted
		}

		// Watch the network before pressing, so the request X makes for this
		// can be waited on rather than raced. Deferred as well as called below
		// because the watch has to be released on the failure paths too.
		settled := settle(p)
		defer settled()

		if err := pressOnPost(p, postID, xui.SelRepostButton, engagementWait); err != nil {
			return err
		}
		// The confirmation is a menu item, which X renders outside the post's
		// article, so it is the one control here that cannot be scoped to it.
		if err := press(p, xui.SelRepostConfirm, engagementWait); err != nil {
			return err
		}
		if !hasOnPost(p, postID, xui.SelUnrepostButton, engagementWait) {
			return errors.New("repost did not take effect: X never showed it as applied")
		}
		settled()

		return confirmApplied(p, target, postID, xui.SelUnrepostButton, ActionRepost)
	})
}

// Bookmark saves a post.
func (w *Writer) Bookmark(ctx context.Context, handle, postID, confirm string) error {
	return w.tap(ctx, ActionBookmark, handle, postID, confirm, xui.SelBookmarkAdd, xui.SelBookmarkRemove)
}

// Unbookmark removes a post from the bookmarks. X toggles the same control, so
// this is Bookmark with the two selectors the other way round.
func (w *Writer) Unbookmark(ctx context.Context, handle, postID, confirm string) error {
	return w.tap(ctx, ActionUnbookmark, handle, postID, confirm, xui.SelBookmarkRemove, xui.SelBookmarkAdd)
}

// tap is the shared shape for single-button actions.
//
// alreadyDone is the selector X swaps in once the action has been applied. When
// it is already present the action is treated as a success rather than pressed
// again, so re-liking an already-liked post does not silently un-like it; and it
// is what every check below reads, so it is required rather than optional.
func (w *Writer) tap(ctx context.Context, action, handle, postID, confirm, button, alreadyDone string) error {
	target, err := postTarget(handle, postID)
	if err != nil {
		return err
	}
	return w.do(ctx, Record{Action: action, Target: target}, confirm, func(p *browser.Page) error {
		if err := p.Goto(target); err != nil {
			return err
		}
		if hasOnPost(p, postID, alreadyDone, appliedWait) {
			return nil
		}

		// Watch the network before pressing, so the request X makes for this
		// can be waited on rather than raced. Deferred as well as called below
		// because the watch has to be released on the failure paths too.
		settled := settle(p)
		defer settled()

		if err := pressOnPost(p, postID, button, engagementWait); err != nil {
			return err
		}
		// A click is not the action. X applies these over the network and swaps
		// the control when it lands, so a click that was accepted locally and
		// never reached X looks identical to one that worked -- and the browser
		// is torn down immediately after this returns, which is enough to lose
		// a request still in flight. Wait for the control to flip.
		if !hasOnPost(p, postID, alreadyDone, engagementWait) {
			return fmt.Errorf("%s did not take effect: X never showed it as applied", action)
		}
		// The control flipping is not the action either. X updates it
		// optimistically, before its request has completed, so anything that
		// disturbs the page at that moment -- tearing the browser down, or
		// navigating, including the reload below -- cancels the request and
		// leaves a page that looked right and changed nothing.
		settled()

		return confirmApplied(p, target, postID, alreadyDone, action)
	})
}

// postTarget validates the pair every action against an existing post needs,
// and returns that post's address.
//
// The id is checked for shape rather than only for emptiness because the page
// script matches it against the digits in a status link: an id of any other
// shape cannot match, and would quietly settle for the first post on the page
// instead of failing.
func postTarget(handle, postID string) (string, error) {
	h := xui.NormalizeHandle(handle)
	if h == "" {
		return "", invalid("handle is required")
	}
	if !xui.ValidPostID(postID) {
		return "", invalid("post id must be the digits X gives a post, got %q", postID)
	}
	return xui.PostURL(h, postID), nil
}

// confirmApplied reloads the post and checks the action survived, which is the
// only check that distinguishes what X has recorded from what its page is
// merely showing.
func confirmApplied(p *browser.Page, target, postID, alreadyDone, action string) error {
	if err := p.Goto(target); err != nil {
		return fmt.Errorf("confirm %s: %w", action, err)
	}
	if !hasOnPost(p, postID, alreadyDone, engagementWait) {
		return fmt.Errorf("%s did not stick: X did not show it as applied after reloading the post", action)
	}
	return nil
}

// engagementWait bounds how long to wait for X to apply a like, repost or
// bookmark. It is a ceiling: the check returns as soon as the control flips.
const engagementWait = 10 * time.Second

// appliedWait bounds the check for an action X has already applied. It is short
// because it runs before anything has been pressed, on a page that is loaded.
const appliedWait = 2 * time.Second

// settle starts watching the page's network so the request an action makes can
// be waited on rather than raced, and returns the wait for it.
//
// Two things about that wait matter. It is bounded: rod takes the quiet interval
// as a minimum rather than a maximum, and x.com is rarely quiet for long, so left
// to run it would spend the rest of the write's budget and then report a failure
// for an action X had in fact applied. Giving up on the wait is the lesser
// problem -- it returns to racing the request, which is where this started.
//
// And it has to run on every path, including the failures: starting the watch
// enables Chrome's Network domain and subscribes to its events, and only the wait
// releases them. So it is safe to defer and to call again, and the second call
// does nothing.
// The wait reports whether the page did go quiet, so a caller with nothing else
// to check on can say so rather than treat giving up as success.
func settle(p *browser.Page) func() bool {
	idle := p.Rod().WaitRequestIdle(requestQuiet, nil, nil, nil)

	var (
		once  sync.Once
		quiet bool
	)
	return func() bool {
		once.Do(func() {
			done := make(chan struct{})
			// rod's wait takes the page's context and no deadline of its own, so
			// the bound is here rather than on the watch -- and it has to start
			// now rather than when the watch opened, since the wait for the
			// control to flip happens in between. A wait abandoned this way ends
			// with the page, which is the end of this write.
			go func() { defer close(done); idle() }()

			select {
			case <-done:
				quiet = true
			case <-time.After(settleWait):
			}
		})
		return quiet
	}
}

// requestQuiet is how long the page's network has to stay quiet before X's
// request for an action is taken to have finished.
const requestQuiet = time.Second

// settleWait bounds the wait for that quiet.
const settleWait = 8 * time.Second

// pollInterval paces the waits that have to ask the page repeatedly.
const pollInterval = 200 * time.Millisecond

// do runs the gate, budget and auth checks, performs the action, and records
// the outcome whichever way it goes.
func (w *Writer) do(ctx context.Context, rec Record, confirm string, action func(*browser.Page) error) error {
	if err := w.gate.Check(confirm); err != nil {
		rec.Outcome = OutcomeDenied
		rec.Reason = err.Error()
		_ = w.audit.Log(rec)
		return err
	}

	if err := w.auth.Require(ctx); err != nil {
		return w.fail(rec, err)
	}
	if err := w.budget.Wait(ctx); err != nil {
		return w.fail(rec, err)
	}

	ctx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()

	// A write needs its own visible browser, and only one Chrome may hold the
	// profile. The reservation is held until the action finishes, so a read
	// cannot warm a second browser on the directory mid-write.
	var reservation pool.Reservation
	if w.reserve != nil {
		got, err := w.reserve.Reserve(ctx)
		if err != nil {
			return w.fail(rec, err)
		}
		reservation = got
	}

	// Writes run in a visible browser: X guards its compose and engagement
	// controls more aggressively than its timelines, and a headless window is
	// both likelier to be refused and impossible for the user to observe.
	session, err := w.open(ctx, false)
	if err != nil {
		releaseProfile(reservation)
		return w.fail(rec, err)
	}
	// The profile is handed back only once this browser is confirmed gone. Its
	// shutdown reports whether Chrome actually exited, and releasing on an
	// unconfirmed one would invite a read onto a directory the write browser
	// may still hold.
	defer func() { releaseAfterShutdown(session, reservation) }()

	page, err := session.Page(ctx)
	if err != nil {
		return w.fail(rec, err)
	}
	defer page.Close()

	if err := action(page); err != nil {
		return w.fail(rec, err)
	}

	rec.Outcome = OutcomeOK
	_ = w.audit.Log(rec)
	if w.onChange != nil {
		w.onChange()
	}
	return nil
}

// releaseAfterShutdown closes the write browser and only then hands the profile
// back.
//
// If the browser cannot be confirmed gone, the reservation is kept while a
// background wait watches for the profile to actually free up. Holding it keeps
// reads off a directory that may still be owned; the bound keeps a wedged
// browser from making the service unavailable forever.
func releaseAfterShutdown(session *browser.Session, reservation pool.Reservation) {
	holdUntilFree(session.Close(), session.ProfileDir(), reservation)
}

// holdUntilFree implements that policy against the two things it actually
// depends on: whether the shutdown was confirmed, and which profile to watch.
func holdUntilFree(err error, profileDir string, reservation pool.Reservation) {
	if err == nil {
		releaseProfile(reservation)
		return
	}

	slog.Warn("write browser did not confirm shutdown; holding the profile until it does", "err", err)
	go func() {
		defer releaseProfile(reservation)

		// Ask the profile directory, not the session. Close has already torn
		// the CDP connection down, so the session reports itself dead whether
		// or not Chrome is still running -- and believing it would hand the
		// profile to a read while the write browser still held it. The lock
		// names the process; the process either exists or it does not.
		if err := browser.WaitUntilFree(context.Background(), profileDir, unconfirmedHold); err != nil {
			slog.Warn("write browser still holds the profile; releasing the reservation anyway",
				"after", unconfirmedHold, "err", err)
		}
	}()
}

func releaseProfile(reservation pool.Reservation) {
	if reservation != nil {
		reservation.Release()
	}
}

// unconfirmedHold bounds how long the profile is withheld while waiting on a
// write browser that would not confirm its exit.
const unconfirmedHold = 2 * time.Minute

func (w *Writer) fail(rec Record, err error) error {
	rec.Outcome = OutcomeFailed
	rec.Reason = err.Error()
	_ = w.audit.Log(rec)
	return err
}

// InvalidError marks a request the caller got wrong, as distinct from a write
// that was attempted and did not take effect. The two deserve different
// answers: one is worth correcting and sending again, the other is not.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return e.Reason }

func invalid(format string, a ...any) error {
	return &InvalidError{Reason: fmt.Sprintf(format, a...)}
}

// ValidateText rejects post text X would not accept.
func ValidateText(text string) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return invalid("post text is required")
	}
	if n := len([]rune(trimmed)); n > MaxPostRunes {
		return invalid("post is %d characters; the limit is %d", n, MaxPostRunes)
	}
	return nil
}

// compose types text into the composer and submits it.
func compose(p *browser.Page, text string) error {
	box, err := p.Rod().Element(xui.SelComposeBox)
	if err != nil {
		return fmt.Errorf("compose box not found: %w", err)
	}

	// Click before typing. A reply composer sits collapsed until it is focused,
	// and text entered into an unfocused one never reaches X's editor: the box
	// appears to fill, the submit button stays disabled, and the click that
	// follows lands on a control that cannot be pressed.
	if err := box.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("focus compose box: %w", err)
	}
	if err := box.Input(text); err != nil {
		return fmt.Errorf("enter post text: %w", err)
	}

	// Wait for X to accept the text rather than assuming it did. The submit
	// button is disabled until then, so this doubles as confirmation that the
	// composer actually holds what we typed.
	if err := waitEnabled(p, xui.SelComposeButton, composeWait); err != nil {
		return err
	}

	// Watch the network before submitting. A post is the same shape of problem
	// as a like: X accepts the click and sends the post afterwards, and the tab
	// is discarded as soon as this returns, which is enough to lose a request
	// still in flight. There is nothing to reload to confirm it -- a new post
	// has no address yet -- so this wait is what the write rests on.
	settled := settle(p)
	defer settled()

	if err := click(p, xui.SelComposeButton); err != nil {
		return err
	}
	if !settled() {
		// Not treated as a failure: X had accepted the text and the submit went
		// through, and refusing a post over a page that would not go quiet would
		// be its own kind of wrong answer. But it is the one write with nothing
		// to reload and confirm, so the doubt is worth recording.
		slog.Warn("the page never went quiet after submitting; the post may not have been sent",
			"waited", settleWait)
	}
	return nil
}

// composeWait bounds how long to wait for the submit control to become usable.
//
// It is a ceiling, not a pause: waitEnabled returns as soon as the button is
// clickable, which is the normal case and costs a single poll. The bound only
// runs out when the composer never accepted the text, and failing quickly there
// is better than making the caller wait for a write that was never going to go.
const composeWait = 5 * time.Second

// waitEnabled blocks until the control can actually be clicked.
//
// X disables its submit buttons with pointer-events rather than the disabled
// attribute, so a plain click reports a confusing failure about pointer-events
// instead of "there is nothing to post yet".
func waitEnabled(p *browser.Page, selector string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("the post control never became usable within %s; the composer may not have accepted the text", budget)
		}
		if usableWithin(p, selector, remaining) {
			return nil
		}
		time.Sleep(pollInterval)
	}
}

// usableWithin reports whether the control is there and clickable, waiting up to
// budget for it to appear.
//
// The lookup is bounded as well as the loop around it. rod retries a selector it
// cannot find against the page's own context, so an unbounded one would spend
// the whole write's budget on the first attempt and never reach the ceiling the
// caller asked for.
func usableWithin(p *browser.Page, selector string, budget time.Duration) bool {
	timed := p.Rod().Timeout(budget)
	defer timed.CancelTimeout()

	el, err := timed.Element(selector)
	if err != nil {
		return false
	}
	usable, err := el.Eval(`() => {
		const s = getComputedStyle(this);
		return s.pointerEvents !== 'none' &&
			this.getAttribute('aria-disabled') !== 'true' &&
			!this.disabled;
	}`)
	return err == nil && usable.Value.Bool()
}

// pressOnPost activates one post's engagement control, waiting up to budget for
// X to render it.
func pressOnPost(p *browser.Page, postID, selector string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		state, err := onPost(p, postID, selector, true)
		// An ask that failed leaves it unknown whether the click landed, and
		// these controls toggle -- pressing again to find out could undo the
		// thing it was meant to do. Report it instead of retrying.
		if err != nil {
			return fmt.Errorf("press %s: %w", selector, err)
		}
		if state == controlFound {
			return nil
		}
		if time.Now().After(deadline) {
			if state == noPost {
				return fmt.Errorf("press %s: X rendered no post to press it on", selector)
			}
			return fmt.Errorf("press %s: the post does not offer that control", selector)
		}
		time.Sleep(pollInterval)
	}
}

// hasOnPost reports whether one post's action row holds selector, waiting up to
// budget for it.
//
// It is scoped to the post rather than the page for the reason ControlScript
// gives: a permalink renders other people's posts alongside the one asked for,
// each with an action row of its own, so a page-wide check answers for whichever
// of them the document happens to reach first.
func hasOnPost(p *browser.Page, postID, selector string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		if state, err := onPost(p, postID, selector, false); err == nil && state == controlFound {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(pollInterval)
	}
}

// onPost runs the shared lookup behind pressOnPost and hasOnPost.
func onPost(p *browser.Page, postID, selector string, press bool) (string, error) {
	// Bound the ask. rod retries a JS context that has gone away -- a frame
	// reloading underneath it -- against the page's own context, so an unbounded
	// one would outlast the ceiling the caller set and spend the write's budget.
	timed := p.Rod().Timeout(evalWait)
	defer timed.CancelTimeout()

	res, err := timed.Eval(xui.ControlScript, postID, selector, press)
	if err != nil {
		return "", err
	}
	return res.Value.String(), nil
}

// evalWait bounds a single ask of the page.
const evalWait = 5 * time.Second

// What ControlScript reports back. Finding the control and pressing it share a
// value, since one that could be pressed was by definition found.
const (
	noPost       = "no-post"
	controlFound = "ok"
)

// press activates a control by dispatching a DOM click.
//
// X ignores synthesized mouse events on its engagement controls: a like click
// delivered through CDP is accepted by the page, reports no error, and does
// nothing at all -- the button never flips and no request is made. A direct
// click on the element does work, and persists.
//
// This is for the controls X renders outside a post's article, which cannot be
// scoped to one: the repost confirmation lives in a menu of its own. Composing
// is left on the mouse-event path, which X does honour there and which is closer
// to what a person does.
//
// The lookup is bounded, since rod would otherwise retry a control that never
// appears until the whole write timed out.
func press(p *browser.Page, selector string, budget time.Duration) error {
	timed := p.Rod().Timeout(budget)
	defer timed.CancelTimeout()

	el, err := timed.Element(selector)
	if err != nil {
		return fmt.Errorf("control not found (%s): %w", selector, err)
	}
	if _, err := el.Eval(`() => this.click()`); err != nil {
		return fmt.Errorf("press %s: %w", selector, err)
	}
	return nil
}

func click(p *browser.Page, selector string) error {
	el, err := p.Rod().Element(selector)
	if err != nil {
		return fmt.Errorf("control not found (%s): %w", selector, err)
	}
	if err := el.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return fmt.Errorf("click %s: %w", selector, err)
	}
	return nil
}
