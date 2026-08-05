// Package write implements the mutating X actions, behind explicit gating.
//
// Nothing here is destructive: there is no delete, unfollow, block or DM, so
// the worst outcome of a mistake is something the user can undo by hand.
package write

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-rod/rod/lib/proto"

	"github.com/SohrabZ/x-browser-mcp/internal/auth"
	"github.com/SohrabZ/x-browser-mcp/internal/browser"
	"github.com/SohrabZ/x-browser-mcp/internal/limit"
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

// Writer performs mutating actions.
type Writer struct {
	open    Opener
	auth    *auth.Manager
	gate    *Gate
	budget  *limit.Budget
	audit   *Auditor
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
		if err := click(p, xui.SelRepostButton); err != nil {
			return err
		}
		return click(p, xui.SelRepostConfirm)
	})
}

// Bookmark saves a post.
func (w *Writer) Bookmark(ctx context.Context, handle, postID, confirm string) error {
	return w.tap(ctx, ActionBookmark, handle, postID, confirm, xui.SelBookmarkAdd, "")
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
		return click(p, button)
	})
}

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

	// Writes run in a visible browser: X guards its compose and engagement
	// controls more aggressively than its timelines, and a headless window is
	// both likelier to be refused and impossible for the user to observe.
	session, err := w.open(ctx, false)
	if err != nil {
		return w.fail(rec, err)
	}
	defer session.Close()

	page, err := session.Page()
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
	if err := box.Input(text); err != nil {
		return fmt.Errorf("enter post text: %w", err)
	}
	return click(p, xui.SelComposeButton)
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
