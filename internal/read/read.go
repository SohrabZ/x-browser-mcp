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

// shorten bounds how much of a caller's own input is quoted back at them.
//
// These messages name the URL that was rejected, which is what makes them useful,
// and the URL came from the caller. But one of the readers is a model, and an
// unbounded message is a way to fill its context with a single bad request.
func shorten(message string) string {
	if len(message) <= maxMessage {
		return message
	}
	return message[:maxMessage] + "..."
}

// maxMessage is generous for any real x.com URL and far short of a nuisance.
const maxMessage = 300

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
	cache  *cache[Result]
	notifs *cache[NotificationResult]

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
		cache:   newCache[Result](opts.CacheFor),
		notifs:  newCache[NotificationResult](opts.CacheFor),
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
	r.notifs.Invalidate()
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

// Resolved is what a URL turned out to point at.
//
// The kind is returned rather than inferred. The caller used to work it out by
// checking whether a thread had a root, which is indistinguishable from a thread
// that failed to render -- and there is now a third shape, since a notification
// is neither a post nor a thread.
type Resolved struct {
	Kind          xui.TargetKind      `json:"kind"`
	Posts         *Result             `json:"posts,omitempty"`
	Thread        *model.Thread       `json:"thread,omitempty"`
	Notifications *NotificationResult `json:"notifications,omitempty"`
}

// FromURL reads whatever an x.com URL points at.
//
// Callers paste links rather than assembling handle/id pairs, so this is the
// entry point that matches how the tools are actually used. A post URL yields a
// thread; a profile, list, bookmarks or search URL yields that timeline.
func (r *Reader) FromURL(ctx context.Context, raw string, n int) (Resolved, error) {
	target, err := xui.ParseURL(raw)
	if err != nil {
		// Every way this fails is the caller's URL being wrong -- not an x.com
		// link, no post id in it, a search with no query. Saying so is the whole
		// use of this entry point, since callers paste links rather than assemble
		// handle and id pairs, and an unclassified error would say nothing at all.
		return Resolved{}, invalid("%s", shorten(err.Error()))
	}

	posts := func(res Result, err error) (Resolved, error) {
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Kind: target.Kind, Posts: &res}, nil
	}

	switch target.Kind {
	case xui.TargetPost:
		thread, err := r.Thread(ctx, target.Handle, target.PostID, n)
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Kind: target.Kind, Thread: &thread}, nil
	case xui.TargetProfile:
		return posts(r.UserPosts(ctx, target.Handle, n))
	case xui.TargetList:
		return posts(r.List(ctx, target.ListID, n))
	case xui.TargetBookmarks:
		return posts(r.Bookmarks(ctx, n))
	case xui.TargetHome:
		return posts(r.Home(ctx, n))
	case xui.TargetSearch:
		return posts(r.Search(ctx, Query{Text: target.Query, Limit: n}))
	case xui.TargetMentions:
		return posts(r.Mentions(ctx, n))
	case xui.TargetNotifications:
		res, err := r.Notifications(ctx, n)
		if err != nil {
			return Resolved{}, err
		}
		return Resolved{Kind: target.Kind, Notifications: &res}, nil
	default:
		return Resolved{}, invalid("unsupported URL: %s", raw)
	}
}

// Thread reads a post together with the conversation X shows around it.
//
// The post asked for is found by its id, not by position. A reply's permalink
// renders the posts it answers above it, so the first post on the page is the
// top of the conversation, and taking it as the root answered a link to a reply
// with somebody else's post. Everything above the post is its context and
// everything below is replies.
//
// The limit counts replies. Ancestors are read whatever it is, since stopping at
// n posts counted from the top would spend the limit on context and could stop
// before the post itself was reached.
func (r *Reader) Thread(ctx context.Context, handle, postID string, n int) (model.Thread, error) {
	h := xui.NormalizeHandle(handle)
	if h == "" {
		return model.Thread{}, invalid("handle is required")
	}
	// The id is matched against the digits in a status link, which an id of any
	// other shape can never equal: it would load the right page and then report
	// the post missing from it.
	if !xui.ValidPostID(postID) {
		return model.Thread{}, invalid("post id must be the digits X gives a post, got %q", shorten(postID))
	}
	n = ClampLimit(n)

	key := cacheKey("thread|"+h+"|"+postID, n)
	res, err := r.gather(ctx, key, xui.PostURL(h, postID), 0, threadComplete(postID, n))
	if err != nil {
		return model.Thread{}, err
	}

	thread, found := threadFrom(res.Posts, postID, n)
	if !found {
		// A page that did not render the post is not worth keeping: serving it
		// from the cache would answer every retry the same way for the whole
		// TTL, however soon X would have rendered it.
		r.cache.drop(key)
		// Handing back another post from the page instead would put words in
		// the wrong account's mouth, which is worse than no answer.
		return model.Thread{}, notFound("the post was not among those X rendered for it; it may be deleted, private, or the id may be wrong")
	}
	return thread, nil
}

// threadComplete reports that a collection holds the post and n replies below
// it, which is all a thread read needs; the posts above it come along anyway.
func threadComplete(postID string, n int) func([]model.Post) bool {
	return func(posts []model.Post) bool {
		at := indexOf(posts, postID)
		return at >= 0 && len(posts)-at-1 >= n
	}
}

// threadFrom arranges the posts of a permalink, in page order, around the one
// asked for, keeping at most n replies. It reports false when that post is not
// among them.
func threadFrom(posts []model.Post, postID string, n int) (model.Thread, bool) {
	at := indexOf(posts, postID)
	if at < 0 {
		return model.Thread{}, false
	}
	replies := posts[at+1:]
	if len(replies) > n {
		replies = replies[:n]
	}
	thread := model.Thread{Root: posts[at], Replies: replies}
	if at > 0 {
		thread.Ancestors = posts[:at]
	}
	return thread, true
}

// indexOf finds a post by id, or reports -1.
func indexOf(posts []model.Post, id string) int {
	for i, p := range posts {
		if p.ID == id {
			return i
		}
	}
	return -1
}

// timeline is the shared path behind every surface that returns n posts.
func (r *Reader) timeline(ctx context.Context, key, url string, n int) (Result, error) {
	return r.gather(ctx, key, url, n, func(posts []model.Post) bool { return len(posts) >= n })
}

// gather is cache, budget and auth, then scroll-and-collect until enough says
// the page has given what was asked for or the page stops producing new posts.
// keep caps what is collected; zero keeps everything.
func (r *Reader) gather(ctx context.Context, key, url string, keep int, enough func([]model.Post) bool) (Result, error) {
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

	posts, err := collect(ctx, r, url, keep, enough, scrape, model.Dedupe,
		"no posts found; the account or list may not exist, or X did not render the timeline")
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

// NotificationResult is a page of notifications.
//
// There is no Contributors here as there is for posts: a notification names its
// own actors, and several of them for an aggregated cell, so ranking accounts
// across the page would be counting the same like twice.
type NotificationResult struct {
	Notifications []model.Notification `json:"notifications"`
	FetchedAt     time.Time            `json:"fetched_at"`
	Cached        bool                 `json:"cached"`
}

// Mentions reads the notifications tab that holds posts.
//
// Mentions are ordinary posts, so this is the ordinary post path pointed at a
// different URL. Notifications are not, and take their own.
func (r *Reader) Mentions(ctx context.Context, n int) (Result, error) {
	n = ClampLimit(n)
	return r.timeline(ctx, cacheKey("mentions", n), xui.MentionsURL, n)
}

// Notifications reads the "All" tab: likes, follows, reposts and X's own
// recommendations.
//
// Almost none of these are posts. On a real account, seventeen of eighteen cells
// held no post at all, so reading this page with the post extractor would return
// the one and quietly present it as the lot.
func (r *Reader) Notifications(ctx context.Context, n int) (NotificationResult, error) {
	n = ClampLimit(n)
	key := cacheKey("notifications", n)

	if hit, ok := r.notifs.get(key); ok {
		hit.Cached = true
		return hit, nil
	}

	if err := r.auth.Require(ctx); err != nil {
		return NotificationResult{}, err
	}
	if err := r.budget.Wait(ctx); err != nil {
		return NotificationResult{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	items, err := collect(ctx, r, xui.NotificationsURL, n, func(items []model.Notification) bool { return len(items) >= n },
		scrapeNotifications, model.DedupeNotifications, "no notifications found")
	if err != nil {
		return NotificationResult{}, err
	}

	result := NotificationResult{Notifications: items, FetchedAt: time.Now().UTC()}
	r.notifs.put(key, result)
	return result, nil
}

// collect opens the page and scrolls until enough reports it has what was asked
// for or the timeline stops yielding new items. keep caps what is gathered, and
// zero leaves it uncapped.
//
// It is a function rather than a method, and generic over what it gathers, so the
// notifications surface shares this loop instead of copying it. Only three things
// differ between the surfaces: how a batch is read off the page, how repeats are
// recognised, and what counts as enough -- n items for a timeline, but for a
// thread the post itself and n replies beneath it, however much sits above.
func collect[T any](
	ctx context.Context,
	r *Reader,
	url string,
	keep int,
	enough func([]T) bool,
	read func(*browser.Page) ([]T, error),
	dedupe func([]T, int) []T,
	emptyReason string,
) ([]T, error) {
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
		gathered []T
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

		batch, err := read(page)
		if err != nil && failed == nil {
			failed = err
		}
		if err == nil {
			before := len(gathered)
			gathered = dedupe(append(gathered, batch...), keep)

			if enough(gathered) {
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
		return nil, cameBackEmpty(ctx.Err(), failed, emptyReason)
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
func cameBackEmpty(ctxErr, failed error, reason string) error {
	if ctxErr != nil {
		return ctxErr
	}
	if failed != nil {
		return failed
	}
	return notFound("%s", reason)
}

// scrapeNotifications runs the notification script and converts what it finds.
//
// It takes no limit on purpose. Capping in the page caps DOM nodes before
// repeats and empty cells have been discarded, so a page whose first cells repeat
// would come back short while the rest sat rendered below it. collect applies the
// cap when it dedupes, where what is counted is notifications.
func scrapeNotifications(page *browser.Page) ([]model.Notification, error) {
	value, err := page.Rod().Eval(xui.NotificationScript)
	if err != nil {
		return nil, err
	}

	var raw []xui.RawNotification
	if err := value.Value.Unmarshal(&raw); err != nil {
		return nil, fmt.Errorf("decode scraped notifications: %w", err)
	}
	return xui.ToNotifications(raw), nil
}

// scrape runs the extraction script and converts what it finds. Like
// scrapeNotifications it takes no limit, for the same reason.
func scrape(page *browser.Page) ([]model.Post, error) {
	value, err := page.Rod().Eval(xui.ExtractScript)
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
