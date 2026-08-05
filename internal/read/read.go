// Package read implements the X read surfaces: timelines, search, threads,
// bookmarks and lists.
package read

import (
	"context"
	"fmt"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/auth"
	"github.com/SohrabZ/x-browser-mcp/internal/browser"
	"github.com/SohrabZ/x-browser-mcp/internal/limit"
	"github.com/SohrabZ/x-browser-mcp/internal/model"
	"github.com/SohrabZ/x-browser-mcp/internal/xui"
)

// Result is a set of posts plus the accounts that produced them.
type Result struct {
	Posts        []model.Post        `json:"posts"`
	Contributors []model.Contributor `json:"contributors"`
	FetchedAt    time.Time           `json:"fetched_at"`
	Cached       bool                `json:"cached"`
}

// Query describes a search request.
type Query struct {
	Text  string
	Mode  xui.SearchMode
	Limit int
}

// Limits on how much a single call may ask for.
const (
	DefaultLimit = 8
	MaxLimit     = 50

	maxScrolls    = 12
	settleTimeout = 20 * time.Second
	scrollPause   = 1200 * time.Millisecond

	// stallRounds is how many scrolls may return nothing new before a timeline
	// is treated as exhausted.
	//
	// Lazily rendered entries can miss the first round, so this is not 1. It is
	// kept small on purpose: waiting longer does not make X return replies it
	// has decided not to render, it only makes every short page slow.
	stallRounds = 3
)

// InvalidError marks a request the caller got wrong -- a missing handle, a URL
// this cannot read. It is not a server fault and repeating it will not help.
type InvalidError struct{ Reason string }

func (e *InvalidError) Error() string { return e.Reason }

// NotFoundError marks a surface X had nothing on. A list id that does not exist
// is the caller asking for something absent, not the server failing.
type NotFoundError struct{ Reason string }

func (e *NotFoundError) Error() string { return e.Reason }

func invalid(format string, a ...any) error {
	return &InvalidError{Reason: fmt.Sprintf(format, a...)}
}

func notFound(format string, a ...any) error {
	return &NotFoundError{Reason: fmt.Sprintf(format, a...)}
}

// ClampLimit brings a caller-supplied limit into range.
func ClampLimit(n int) int {
	if n <= 0 {
		return DefaultLimit
	}
	if n > MaxLimit {
		return MaxLimit
	}
	return n
}

// Lease borrows a browser session and returns a function that hands it back.
//
// Reads borrow rather than open so a warm browser can be shared between them:
// launching and quitting Chrome costs about 1.8s per read on its own, before
// the slower cold page loads.
type Lease func(ctx context.Context) (*browser.Session, func(), error)

// Reader reads X through a browser.
type Reader struct {
	lease  Lease
	auth   *auth.Manager
	budget *limit.Budget
	cache  *cache

	timeout time.Duration
}

// Options configures a Reader.
type Options struct {
	Lease    Lease
	Auth     *auth.Manager
	Budget   *limit.Budget
	CacheFor time.Duration
	Timeout  time.Duration
}

// New builds a Reader.
func New(opts Options) *Reader {
	return &Reader{
		lease:   opts.Lease,
		auth:    opts.Auth,
		budget:  opts.Budget,
		cache:   newCache(opts.CacheFor),
		timeout: opts.Timeout,
	}
}

// Home reads the signed-in home timeline.
// Invalidate drops every cached result.
//
// A write changes what the next read should return -- a new post belongs in the
// timeline, a like belongs on the post -- and serving the pre-write copy for
// the rest of the TTL makes the write look like it did not happen.
func (r *Reader) Invalidate() {
	r.cache.Invalidate()
}

func (r *Reader) Home(ctx context.Context, n int) (Result, error) {
	n = ClampLimit(n)
	return r.timeline(ctx, cacheKey("home", n), xui.HomeURL, n)
}

// Search reads recent posts matching a query.
func (r *Reader) Search(ctx context.Context, q Query) (Result, error) {
	if q.Text == "" {
		return Result{}, invalid("search query is required")
	}
	if !q.Mode.Valid() {
		q.Mode = xui.Latest
	}
	n := ClampLimit(q.Limit)

	key := cacheKey(fmt.Sprintf("search|%s|%s", q.Text, q.Mode), n)
	return r.timeline(ctx, key, xui.SearchURL(q.Text, q.Mode), n)
}

// UserPosts reads an account's own timeline.
func (r *Reader) UserPosts(ctx context.Context, handle string, n int) (Result, error) {
	h := xui.NormalizeHandle(handle)
	if h == "" {
		return Result{}, invalid("handle is required")
	}
	n = ClampLimit(n)
	return r.timeline(ctx, cacheKey("user|"+h, n), xui.UserURL(h), n)
}

// Bookmarks reads the signed-in account's saved posts.
func (r *Reader) Bookmarks(ctx context.Context, n int) (Result, error) {
	n = ClampLimit(n)
	return r.timeline(ctx, cacheKey("bookmarks", n), xui.BookmarksURL, n)
}

// List reads a curated list's timeline.
func (r *Reader) List(ctx context.Context, listID string, n int) (Result, error) {
	if listID == "" {
		return Result{}, invalid("list id is required")
	}
	n = ClampLimit(n)
	return r.timeline(ctx, cacheKey("list|"+listID, n), xui.ListURL(listID), n)
}

// FromURL reads whatever an x.com URL points at.
//
// Callers paste links rather than assembling handle/id pairs, so this is the
// entry point that matches how the tools are actually used. A post URL yields a
// thread; a profile, list, bookmarks or search URL yields that timeline.
func (r *Reader) FromURL(ctx context.Context, raw string, n int) (Result, model.Thread, error) {
	target, err := xui.ParseURL(raw)
	if err != nil {
		return Result{}, model.Thread{}, err
	}

	switch target.Kind {
	case xui.TargetPost:
		thread, err := r.Thread(ctx, target.Handle, target.PostID, n)
		return Result{}, thread, err
	case xui.TargetProfile:
		res, err := r.UserPosts(ctx, target.Handle, n)
		return res, model.Thread{}, err
	case xui.TargetList:
		res, err := r.List(ctx, target.ListID, n)
		return res, model.Thread{}, err
	case xui.TargetBookmarks:
		res, err := r.Bookmarks(ctx, n)
		return res, model.Thread{}, err
	case xui.TargetHome:
		res, err := r.Home(ctx, n)
		return res, model.Thread{}, err
	case xui.TargetSearch:
		res, err := r.Search(ctx, Query{Text: target.Query, Limit: n})
		return res, model.Thread{}, err
	default:
		return Result{}, model.Thread{}, invalid("unsupported URL: %s", raw)
	}
}

// Thread reads a post together with the replies shown beneath it.
//
// X renders the root and its replies as the same kind of article, so the first
// post on the page is the root and the rest are replies.
func (r *Reader) Thread(ctx context.Context, handle, postID string, n int) (model.Thread, error) {
	h := xui.NormalizeHandle(handle)
	if h == "" || postID == "" {
		return model.Thread{}, invalid("handle and post id are required")
	}

	res, err := r.timeline(ctx, cacheKey("thread|"+h+"|"+postID, n), xui.PostURL(h, postID), ClampLimit(n))
	if err != nil {
		return model.Thread{}, err
	}
	if len(res.Posts) == 0 {
		return model.Thread{}, notFound("no posts found for that thread; it may be deleted, private, or the id may be wrong")
	}
	return model.Thread{Root: res.Posts[0], Replies: res.Posts[1:]}, nil
}

// timeline is the shared path behind every surface: cache, budget, auth, then
// scroll-and-collect until the target count is reached or the page stops
// producing new posts.
func (r *Reader) timeline(ctx context.Context, key, url string, n int) (Result, error) {
	if hit, ok := r.cache.get(key); ok {
		hit.Cached = true
		return hit, nil
	}

	if err := r.auth.Require(ctx); err != nil {
		return Result{}, err
	}
	if err := r.budget.Wait(ctx); err != nil {
		return Result{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	posts, err := r.collect(ctx, url, n)
	if err != nil {
		return Result{}, err
	}

	result := Result{
		Posts:        posts,
		Contributors: model.Contributors(posts),
		FetchedAt:    time.Now().UTC(),
	}
	r.cache.put(key, result)
	return result, nil
}

// collect opens the page and scrolls until it has enough posts or the timeline
// stops yielding new ones.
func (r *Reader) collect(ctx context.Context, url string, n int) ([]model.Post, error) {
	session, release, err := r.lease(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	page, err := session.Page(ctx)
	if err != nil {
		return nil, err
	}
	defer page.Close()

	if err := page.Goto(url); err != nil {
		return nil, err
	}

	var (
		gathered []model.Post
		stalled  int
		// The first thing that went wrong, kept in case nothing is gathered: an
		// empty timeline and a browser that died look identical from here.
		failed   error
		deadline = time.Now().Add(settleTimeout)
	)

	for scroll := 0; scroll < maxScrolls; scroll++ {
		if err := ctx.Err(); err != nil {
			// Partial results beat nothing when the caller's budget runs out.
			if len(gathered) > 0 {
				return gathered, nil
			}
			return nil, err
		}

		batch, err := scrape(page, n)
		if err != nil && failed == nil {
			failed = err
		}
		if err == nil {
			before := len(gathered)
			gathered = model.Dedupe(append(gathered, batch...), n)

			if len(gathered) >= n {
				return gathered, nil
			}
			if len(gathered) > before {
				stalled = 0
			} else {
				stalled++
			}
		}

		// Repeated empty rounds mean the timeline has given what it has.
		if stalled >= stallRounds && len(gathered) > 0 {
			return gathered, nil
		}
		if time.Now().After(deadline) {
			break
		}

		if _, err := page.Rod().Eval(xui.ScrollScript); err != nil {
			if failed == nil {
				failed = err
			}
			break
		}
		time.Sleep(scrollPause)
	}

	if len(gathered) == 0 {
		return nil, cameBackEmpty(ctx.Err(), failed)
	}
	return gathered, nil
}

// cameBackEmpty decides what a read that gathered nothing should report.
//
// Nothing gathered has three causes that look identical from the end of the
// loop: the caller's budget ran out, something broke while reading, or the
// timeline genuinely had nothing. Only the third is "not found", and answering
// that for either of the others sends the caller looking for a post that may
// well exist while hiding the fault that stopped it being found.
func cameBackEmpty(ctxErr, failed error) error {
	if ctxErr != nil {
		return ctxErr
	}
	if failed != nil {
		return failed
	}
	return notFound("no posts found; the account or list may not exist, or X did not render the timeline")
}

// scrape runs the extraction script and converts what it finds.
func scrape(page *browser.Page, n int) ([]model.Post, error) {
	value, err := page.Rod().Eval(xui.ExtractScript, n)
	if err != nil {
		return nil, err
	}

	var raw []xui.RawPost
	if err := value.Value.Unmarshal(&raw); err != nil {
		return nil, fmt.Errorf("decode scraped posts: %w", err)
	}
	return xui.ToPosts(raw), nil
}

func cacheKey(prefix string, n int) string {
	return fmt.Sprintf("%s|%d", prefix, n)
}
