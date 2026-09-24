// Package write implements the mutating X actions, behind explicit gating.
//
// Nothing here is destructive: there is no delete, unfollow, block or DM, so
// the worst outcome of a mistake is something the user can undo by hand.
package write

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
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

// Actions is the mutating surface a transport needs.
//
// It exists so the MCP tools and the HTTP routes can be tested without a
// browser. What those layers are responsible for is decoding a request and
// calling the right action, and a real Writer makes exactly that impossible to
// assert -- every call would drive Chrome at X.
type Actions interface {
	Enabled() bool
	AutoApproved() bool
	Post(ctx context.Context, text, confirm string) error
	Reply(ctx context.Context, handle, postID, text, confirm string) error
	Like(ctx context.Context, handle, postID, confirm string) error
	Repost(ctx context.Context, handle, postID, confirm string) error
	Bookmark(ctx context.Context, handle, postID, confirm string) error
	Unbookmark(ctx context.Context, handle, postID, confirm string) error
}

// Writer implements it.
var _ Actions = (*Writer)(nil)

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
//
// Safe on a nil Writer, because the transports hold one as an Actions and a nil
// pointer in an interface is not a nil interface: their "no writer configured"
// check cannot catch it, so answering honestly here is what keeps a missing
// writer from being a panic at startup.
func (w *Writer) Enabled() bool { return w != nil && w.gate.Enabled() }

// AutoApproved reports whether writes go ahead without an approval code. The
// transports describe the write tools differently when they do, since telling a
// model to fetch a code nobody will ask for is a prompt that misleads it.
func (w *Writer) AutoApproved() bool { return w != nil && w.gate.AutoApproved() }

// Post publishes a new post.
func (w *Writer) Post(ctx context.Context, text, confirm string) error {
	if err := ValidateText(text); err != nil {
		return err
	}
	return w.do(ctx, Record{Action: ActionPost, Excerpt: text}, confirm, func(p *browser.Page) (string, error) {
		if err := p.Goto(xui.HomeURL); err != nil {
			return "", err
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
	return w.do(ctx, Record{Action: ActionReply, Target: target, Excerpt: text}, confirm, func(p *browser.Page) (string, error) {
		if err := p.Goto(target); err != nil {
			return "", err
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
	return w.do(ctx, Record{Action: ActionRepost, Target: target}, confirm, engagement(func(p *browser.Page) error {
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
			return notApplied("repost did not take effect: X never showed it as applied")
		}
		settled()

		return confirmApplied(p, target, postID, xui.SelUnrepostButton, ActionRepost)
	}))
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
	return w.do(ctx, Record{Action: action, Target: target}, confirm, engagement(func(p *browser.Page) error {
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
			return notApplied("%s did not take effect: X never showed it as applied", action)
		}
		// The control flipping is not the action either. X updates it
		// optimistically, before its request has completed, so anything that
		// disturbs the page at that moment -- tearing the browser down, or
		// navigating, including the reload below -- cancels the request and
		// leaves a page that looked right and changed nothing.
		settled()

		return confirmApplied(p, target, postID, alreadyDone, action)
	}))
}

// engagement adapts an action that acts on an existing post, and so creates
// nothing, to the shape do takes.
func engagement(act func(*browser.Page) error) func(*browser.Page) (string, error) {
	return func(p *browser.Page) (string, error) { return "", act(p) }
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
		return notApplied("%s did not stick: X did not show it as applied after reloading the post", action)
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
// the outcome whichever way it goes. An action that publishes something reports
// the id X gave it, which the record keeps.
func (w *Writer) do(ctx context.Context, rec Record, confirm string, action func(*browser.Page) (string, error)) error {
	// The record's excerpt is still the whole text here; the auditor cuts it
	// only when writing the line. The approval has to bind all of it.
	if err := w.gate.Check(Request{Action: rec.Action, Target: rec.Target, Text: rec.Excerpt}, confirm); err != nil {
		// Asking for approval is how every write starts, so it is recorded as
		// its own outcome: a burst of denials is the sign of something trying
		// codes, and ordinary use should not look like one.
		rec.Outcome = OutcomeDenied
		if errors.Is(err, ErrApprovalRequired) {
			rec.Outcome = OutcomePending
		}
		rec.Reason = err.Error()
		_ = w.audit.Log(rec)
		return err
	}
	// Every line from here on says whether a person approved this write or the
	// server let it through, since that is the first question after one that
	// should not have happened.
	rec.AutoApproved = w.gate.AutoApproved()

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

	created, err := action(page)
	if err != nil {
		return w.fail(rec, err)
	}

	rec.Created = created
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

// NotFoundError marks a post X did not render. Liking a deleted or private post
// is not a fault of this server's, and the caller needs to know the target is
// gone rather than be told to try again.
type NotFoundError struct{ Reason string }

func (e *NotFoundError) Error() string { return e.Reason }

// NotAppliedError marks an action that was carried out and that X did not apply.
//
// Distinct from a fault: the machinery worked, so what it says is an answer the
// caller can act on rather than a detail of this process. It says only what X
// showed, and carries nothing else.
type NotAppliedError struct{ Reason string }

func (e *NotAppliedError) Error() string { return e.Reason }

func notApplied(format string, a ...any) error {
	return &NotAppliedError{Reason: fmt.Sprintf(format, a...)}
}

// UnconfirmedError marks a post or reply that was submitted and that X never
// answered. It may or may not have been published, and saying which would be a
// guess, so it says that instead: a caller told "failed" would send it again,
// and one told "posted" would never check.
type UnconfirmedError struct{ Reason string }

func (e *UnconfirmedError) Error() string { return e.Reason }

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
func compose(p *browser.Page, text string) (string, error) {
	box, err := p.Rod().Element(xui.SelComposeBox)
	if err != nil {
		return "", fmt.Errorf("compose box not found: %w", err)
	}

	// Click before typing. A reply composer sits collapsed until it is focused,
	// and text entered into an unfocused one never reaches X's editor: the box
	// appears to fill, the submit button stays disabled, and the click that
	// follows lands on a control that cannot be pressed.
	if err := box.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return "", fmt.Errorf("focus compose box: %w", err)
	}
	if err := box.Input(text); err != nil {
		return "", fmt.Errorf("enter post text: %w", err)
	}

	// Wait for X to accept the text rather than assuming it did. The submit
	// button is disabled until then, so this doubles as confirmation that the
	// composer actually holds what we typed.
	if err := waitEnabled(p, xui.SelComposeButton, composeWait); err != nil {
		return "", err
	}

	// Listen before submitting. A post is the same shape of problem as a like:
	// X accepts the click and sends the post afterwards, and the tab is discarded
	// as soon as this returns, which is enough to lose a request still in
	// flight. And there is nothing to reload to confirm it, since a new post has
	// no address until X gives it one. So X's answer to the request is what the
	// write rests on.
	//
	// It used to rest on the page going quiet, and a page that never did was
	// reported as posted anyway. That answered "Posted." for a post X had refused
	// -- a duplicate, a rate limit -- and for one it never received.
	answer := watchCreate(p)

	if err := click(p, xui.SelComposeButton); err != nil {
		answer(0)
		return "", err
	}
	return answer(createWait)
}

// createWait bounds the wait for X to answer a post. X answers in about a
// second; this only has to outlast a slow one.
//
// It is a variable so tests can shorten it.
var createWait = 15 * time.Second

// watchCreate listens for X's answer to the request that publishes a post, and
// returns the wait for it. The wait reports the new post's id, or why there is
// none, and stops the listening whichever way it ends; a budget of zero stops it
// at once.
//
// The answer is read where X's own client reads it, so the verdict is the one X
// shows the user: an id means X created the post. Anything short of an answer
// is reported as unconfirmed rather than failed, because the post may well have
// been published, and a caller told it failed would send it again.
func watchCreate(p *browser.Page) func(budget time.Duration) (string, error) {
	page, stop := p.Rod().WithCancel()

	type outcome struct {
		id  string
		err error
	}
	answered := make(chan outcome, 1)

	// Subscribing here, before the submit, is what makes the request visible:
	// events are delivered from the moment of the call, and the network domain
	// is enabled for as long as the listening lasts. The callbacks run one at a
	// time, so request needs no lock.
	var request proto.NetworkRequestID
	listen := page.EachEvent(
		func(e *proto.NetworkRequestWillBeSent) {
			if request == "" && e.Request.Method == "POST" && xui.IsCreatePost(e.Request.URL) {
				request = e.RequestID
			}
		},
		func(e *proto.NetworkLoadingFinished) bool {
			if request == "" || e.RequestID != request {
				return false
			}
			id, err := readCreated(page, request)
			answered <- outcome{id, err}
			return true
		},
		func(e *proto.NetworkLoadingFailed) bool {
			if request == "" || e.RequestID != request {
				return false
			}
			// The browser lost the answer, which says nothing about whether X
			// received the request.
			slog.Warn("the request publishing a post failed in the browser", "err", e.ErrorText)
			answered <- outcome{err: unconfirmed()}
			return true
		},
	)
	go listen()

	return func(budget time.Duration) (string, error) {
		defer stop()
		select {
		case got := <-answered:
			return got.id, got.err
		case <-time.After(budget):
			return "", unconfirmed()
		}
	}
}

// readCreated reads X's answer to the request that published a post.
func readCreated(page *rod.Page, request proto.NetworkRequestID) (string, error) {
	res, err := proto.NetworkGetResponseBody{RequestID: request}.Call(page)
	if err != nil {
		slog.Warn("X answered the post, but the answer could not be read", "err", err)
		return "", unconfirmed()
	}
	body := []byte(res.Body)
	if res.Base64Encoded {
		if body, err = base64.StdEncoding.DecodeString(res.Body); err != nil {
			slog.Warn("X's answer to the post could not be decoded", "err", err)
			return "", unconfirmed()
		}
	}

	id, refusal := xui.CreatedPost(body)
	switch {
	case id != "":
		return id, nil
	case refusal != "":
		return "", notApplied("X did not publish it: %s", refusal)
	default:
		return "", unconfirmed()
	}
}

// unconfirmed is what a post X never answered is reported as.
func unconfirmed() error {
	return &UnconfirmedError{Reason: fmt.Sprintf("X did not confirm the post within %s. It may have been "+
		"published, so check the account before sending it again", createWait)}
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
				// The address loaded and X put no post on it. That is the
				// caller's answer, not a fault: the post is deleted, private, or
				// the id is wrong. The selector is no part of it.
				return &NotFoundError{Reason: "no post at that address; it may be deleted, private, or the id may be wrong"}
			}
			// The post is there and the control is not, which is either X
			// withholding it or this server's selectors having drifted. Those are
			// not distinguishable from here, and one of them is a fault, so this
			// stays unclassified and goes to the log.
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
