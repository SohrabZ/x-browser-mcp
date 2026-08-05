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
	ActionPost     = "post"
	ActionReply    = "reply"
	ActionLike     = "like"
	ActionRepost   = "repost"
	ActionBookmark = "bookmark"
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
	h := xui.NormalizeHandle(handle)
	if h == "" || postID == "" {
		return errors.New("handle and post id are required")
	}
	if err := ValidateText(text); err != nil {
		return err
	}

	target := xui.PostURL(h, postID)
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
	h := xui.NormalizeHandle(handle)
	if h == "" || postID == "" {
		return errors.New("handle and post id are required")
	}
	target := xui.PostURL(h, postID)

	return w.do(ctx, Record{Action: ActionRepost, Target: target}, confirm, func(p *browser.Page) error {
		if err := p.Goto(target); err != nil {
			return err
		}
		if p.Has(xui.SelUnrepostButton, 2*time.Second) {
			return nil // already reposted
		}
		if err := press(p, xui.SelRepostButton); err != nil {
			return err
		}
		if err := press(p, xui.SelRepostConfirm); err != nil {
			return err
		}
		if !p.Has(xui.SelUnrepostButton, engagementWait) {
			return errors.New("repost did not take effect: X still shows the action as available")
		}
		return confirmApplied(p, target, xui.SelUnrepostButton, ActionRepost)
	})
}

// Bookmark saves a post.
func (w *Writer) Bookmark(ctx context.Context, handle, postID, confirm string) error {
	return w.tap(ctx, ActionBookmark, handle, postID, confirm, xui.SelBookmarkAdd, xui.SelBookmarkRemove)
}

// tap is the shared shape for single-button actions.
//
// alreadyDone is the selector X swaps in once the action has been applied; when
// it is present the action is treated as a success rather than clicked again,
// so re-liking an already-liked post does not silently un-like it.
func (w *Writer) tap(ctx context.Context, action, handle, postID, confirm, button, alreadyDone string) error {
	h := xui.NormalizeHandle(handle)
	if h == "" || postID == "" {
		return errors.New("handle and post id are required")
	}
	target := xui.PostURL(h, postID)

	return w.do(ctx, Record{Action: action, Target: target}, confirm, func(p *browser.Page) error {
		if err := p.Goto(target); err != nil {
			return err
		}
		if alreadyDone != "" && p.Has(alreadyDone, 2*time.Second) {
			return nil
		}
		// Start watching for network activity before pressing, so the request
		// X makes for this action can be waited on rather than raced.
		settled := p.Rod().WaitRequestIdle(time.Second, nil, nil, nil)

		if err := press(p, button); err != nil {
			return err
		}
		// A click is not the action. X applies these over the network and swaps
		// the control when it lands, so a click that was accepted locally and
		// never reached X looks identical to one that worked -- and the browser
		// is torn down immediately after this returns, which is enough to lose
		// a request still in flight. Wait for the control to flip.
		if alreadyDone == "" {
			return nil
		}
		if !p.Has(alreadyDone, engagementWait) {
			return fmt.Errorf("%s did not take effect: X still shows the action as available", action)
		}
		// The control flipping is not the action either. X updates it
		// optimistically, before its request has completed, so anything that
		// disturbs the page at that moment -- tearing the browser down, or
		// navigating, including the reload below -- cancels the request and
		// leaves a page that looked right and changed nothing.
		settled()

		return confirmApplied(p, target, alreadyDone, action)
	})
}

// confirmApplied reloads the post and checks the action survived, which is the
// only check that distinguishes what X has recorded from what its page is
// merely showing.
func confirmApplied(p *browser.Page, target, alreadyDone, action string) error {
	if err := p.Goto(target); err != nil {
		return fmt.Errorf("confirm %s: %w", action, err)
	}
	if !p.Has(alreadyDone, engagementWait) {
		return fmt.Errorf("%s did not stick: X shows the action as still available after reloading the post", action)
	}
	return nil
}

// engagementWait bounds how long to wait for X to apply a like, repost or
// bookmark. It is a ceiling: the check returns as soon as the control flips.
const engagementWait = 10 * time.Second

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

// ValidateText rejects post text X would not accept.
func ValidateText(text string) error {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return errors.New("post text is required")
	}
	if n := len([]rune(trimmed)); n > MaxPostRunes {
		return fmt.Errorf("post is %d characters; the limit is %d", n, MaxPostRunes)
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
	return click(p, xui.SelComposeButton)
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
		el, err := p.Rod().Element(selector)
		if err == nil {
			usable, evalErr := el.Eval(`() => {
				const s = getComputedStyle(this);
				return s.pointerEvents !== 'none' &&
					this.getAttribute('aria-disabled') !== 'true' &&
					!this.disabled;
			}`)
			if evalErr == nil && usable.Value.Bool() {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the post control never became usable within %s; the composer may not have accepted the text", budget)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// press activates a control by dispatching a DOM click.
//
// X ignores synthesized mouse events on its engagement controls: a like click
// delivered through CDP is accepted by the page, reports no error, and does
// nothing at all -- the button never flips and no request is made. A direct
// click on the element does work, and persists.
//
// Composing is left on the mouse-event path, which X does honour there and
// which is closer to what a person does.
func press(p *browser.Page, selector string) error {
	el, err := p.Rod().Element(selector)
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
