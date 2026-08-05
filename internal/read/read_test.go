package read

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/browser"
	"github.com/SohrabZ/x-browser-mcp/internal/model"
)

func TestClampLimit(t *testing.T) {
	cases := map[int]int{
		0:            DefaultLimit,
		-5:           DefaultLimit,
		3:            3,
		MaxLimit:     MaxLimit,
		MaxLimit + 1: MaxLimit,
		10_000:       MaxLimit,
	}
	for in, want := range cases {
		if got := ClampLimit(in); got != want {
			t.Errorf("ClampLimit(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestCacheRoundTrips(t *testing.T) {
	c := newCache[Result](time.Minute)
	want := Result{Posts: []model.Post{{ID: "1"}}}

	c.put("k", want)

	got, ok := c.get("k")
	if !ok {
		t.Fatal("expected a cache hit")
	}
	if len(got.Posts) != 1 || got.Posts[0].ID != "1" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

func TestCacheMissesOnUnknownKey(t *testing.T) {
	c := newCache[Result](time.Minute)
	if _, ok := c.get("nope"); ok {
		t.Fatal("expected a miss")
	}
}

func TestCacheExpires(t *testing.T) {
	c := newCache[Result](time.Millisecond)
	c.put("k", Result{})

	time.Sleep(5 * time.Millisecond)

	if _, ok := c.get("k"); ok {
		t.Fatal("expected the entry to have expired")
	}
}

// A zero TTL disables caching outright rather than caching forever, which is
// the dangerous reading of "no expiry".
func TestZeroTTLDisablesCaching(t *testing.T) {
	c := newCache[Result](0)
	c.put("k", Result{})

	if _, ok := c.get("k"); ok {
		t.Fatal("a zero TTL must not cache")
	}
}

func TestInvalidateClearsEverything(t *testing.T) {
	c := newCache[Result](time.Minute)
	c.put("a", Result{})
	c.put("b", Result{})

	c.Invalidate()

	if _, ok := c.get("a"); ok {
		t.Error("expected a to be cleared")
	}
	if _, ok := c.get("b"); ok {
		t.Error("expected b to be cleared")
	}
}

// Cache keys must separate surfaces and sizes, or a 5-post home read would be
// served from a 5-post bookmarks read.
func TestCacheKeysAreDistinctPerSurfaceAndSize(t *testing.T) {
	seen := map[string]bool{}
	keys := []string{
		cacheKey("home", 5),
		cacheKey("home", 10),
		cacheKey("bookmarks", 5),
		cacheKey("user|sohrab", 5),
		cacheKey("user|someone", 5),
		cacheKey("search|go|latest", 5),
		cacheKey("search|go|top", 5),
	}
	for _, k := range keys {
		if seen[k] {
			t.Fatalf("duplicate cache key: %q", k)
		}
		seen[k] = true
	}
}

func TestConcurrentCacheAccessIsSafe(t *testing.T) {
	c := newCache[Result](time.Minute)
	done := make(chan struct{})

	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 100; j++ {
				c.put("k", Result{})
				_, _ = c.get("k")
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// A write changes what the next read should return, so the reader exposes a way
// to drop what it has. The cache had this from the start; the reader did not,
// which meant nothing could actually call it.
func TestReaderInvalidateDropsCachedResults(t *testing.T) {
	r := New(Options{CacheFor: time.Hour})

	r.cache.put("home:20", Result{Posts: []model.Post{{ID: "1", Text: "before"}}})
	if _, ok := r.cache.get("home:20"); !ok {
		t.Fatal("expected the result to be cached")
	}

	r.Invalidate()

	if _, ok := r.cache.get("home:20"); ok {
		t.Fatal("a write invalidated the cache; the stale result is still being served")
	}
}

// A read that broke must not be reported as a read that found nothing. The two
// look identical at the end of the collect loop -- no posts either way -- and
// answering "no posts found" for the first sends the caller looking for a post
// that may well exist, while hiding the fault that stopped it being found.
func TestAReadThatBrokeIsNotReportedAsEmpty(t *testing.T) {
	broke := errors.New("cdp connection closed")

	cases := []struct {
		name    string
		ctxErr  error
		failed  error
		wantErr error
		absent  bool
	}{
		{name: "genuinely empty", absent: true},
		{name: "something broke", failed: broke, wantErr: broke},
		{name: "budget ran out", ctxErr: context.DeadlineExceeded, wantErr: context.DeadlineExceeded},
		{name: "both, deadline wins", ctxErr: context.DeadlineExceeded, failed: broke, wantErr: context.DeadlineExceeded},
	}

	for _, c := range cases {
		got := cameBackEmpty(c.ctxErr, c.failed, "nothing there")

		var missing *NotFoundError
		if absent := errors.As(got, &missing); absent != c.absent {
			t.Errorf("%s: classified as not-found=%v, want %v (%v)", c.name, absent, c.absent, got)
		}
		if c.wantErr != nil && !errors.Is(got, c.wantErr) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.wantErr)
		}
	}
}

// Callers paste links, so a link this cannot read is the most likely mistake
// they will make and the one most worth being told about. Every way ParseURL
// fails is the URL being wrong, and an unclassified error would reach them as
// "internal error" -- which is what this whole classification exists to stop.
func TestABadURLIsTheCallersMistakeNotAFault(t *testing.T) {
	r := New(Options{})

	for _, raw := range []string{
		"",
		"not a url at all",
		"https://example.com/someone/status/222",
		"https://x.com/search",      // no q
		"https://x.com/i/lists/",    // no list id
		"https://x.com/i/somewhere", // an /i/ route this cannot read
		"https://x.com/settings",    // a reserved path that is not a timeline
	} {
		_, err := r.FromURL(context.Background(), raw, 5)
		if err == nil {
			t.Errorf("%q was accepted", raw)
			continue
		}

		var badInput *InvalidError
		if !errors.As(err, &badInput) {
			t.Errorf("%q: got %T (%v), want an InvalidError", raw, err, err)
			continue
		}
		// And it still says which URL, because that is the caller's own input.
		if raw != "" && !strings.Contains(err.Error(), raw) {
			t.Errorf("%q: message %q does not name the URL", raw, err)
		}
	}
}

// One of the readers of these messages is a model, so quoting a caller's input
// back has to be bounded: an unbounded message is a way to fill a context window
// with one bad request.
func TestAnEnormousURLIsNotQuotedBackWhole(t *testing.T) {
	huge := "https://example.com/" + strings.Repeat("a", 20000) // not an x.com URL

	_, err := New(Options{}).FromURL(context.Background(), huge, 5)
	if err == nil {
		t.Fatal("a 20k URL was accepted")
	}
	if n := len(err.Error()); n > maxMessage+len("...") {
		t.Errorf("the message is %d chars; it should be bounded at %d", n, maxMessage)
	}
}

// The notifications page is mostly not posts, which is the whole reason it has
// its own script. This fixture has the shapes a real account produced: one actor
// liking one post, two actors liking one post, one actor liking several, a follow
// with no post at all, X's own recommendation, and a reply that IS a post.
const notificationFixture = `
<div data-testid="cellInnerDiv">
  <div data-testid="notification" role="article">
    <div data-testid="UserAvatar-Container-RamyarKhalili"></div>
    <span>Ramyar Khalili liked your post</span>
    <time datetime="2026-08-05T10:36:27.747Z">7h</time>
    <div data-testid="tweetText">This post was sent using the same MCP server.</div>
  </div>
</div>
<div data-testid="cellInnerDiv">
  <div data-testid="notification" role="article">
    <div data-testid="UserAvatar-Container-RamyarKhalili"></div>
    <div data-testid="UserAvatar-Container-SomniaRobotics"></div>
    <span>Ramyar Khalili and Somnia Lab liked your post</span>
    <time datetime="2026-08-05T10:32:02.300Z">7h</time>
    <div data-testid="tweetText">Reading X through the API is pay-per-use now.</div>
  </div>
</div>
<div data-testid="cellInnerDiv">
  <div data-testid="notification" role="article">
    <div data-testid="UserAvatar-Container-koalchack"></div>
    <span>پانیخذ liked 2 of your posts</span>
    <time datetime="2026-08-05T06:26:23.934Z">11h</time>
    <div data-testid="tweetText">Reading X through the API is pay-per-use now.</div>
  </div>
</div>
<div data-testid="cellInnerDiv">
  <div data-testid="notification" role="article">
    <div data-testid="UserAvatar-Container-SomniaRobotics"></div>
    <span>Somnia Lab followed you</span>
    <time datetime="2026-08-05T09:32:22.394Z">8h</time>
  </div>
</div>
<div data-testid="cellInnerDiv">
  <div data-testid="notification" role="article">
    <div data-testid="UserAvatar-Container-VOAfarsi"></div>
    <span>Recent post from VOA Farsi</span>
    <time datetime="2026-08-04T20:00:00.000Z">22h</time>
    <div data-testid="tweetText">A post this account published.</div>
  </div>
</div>
<div data-testid="cellInnerDiv">
  <article data-testid="tweet">
    <div data-testid="User-Name"><span>پانیخذ</span><span>@koalchack</span></div>
    <a href="/koalchack/status/2084888751042638284"><time datetime="2026-08-05T06:26:28.000Z">11h</time></a>
    <div data-testid="tweetText">Nice</div>
  </article>
</div>`

// scrapeFixture drives a real browser at a page, because a selector cannot be
// tested any other way.
func scrapeFixture(t *testing.T, body string, n int) []model.Notification {
	t.Helper()
	if testing.Short() {
		t.Skip("needs a browser")
	}
	chrome := browser.ChromePathForTest()
	if chrome == "" {
		t.Skip("no Chrome installed")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// With no charset Chrome does not read this as UTF-8 and non-Latin text
		// arrives mangled, which would make this fixture quietly unlike X -- and
		// a lot of what X serves is not Latin.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><head><meta charset=\"utf-8\"></head><body>%s</body></html>", body)
	}))
	defer srv.Close()

	session, err := browser.Open(context.Background(), browser.Options{ChromePath: chrome, Headless: true})
	if err != nil {
		t.Skipf("cannot start chrome: %v", err)
	}
	defer func() { _ = session.Close() }()

	page, err := session.Page(context.Background())
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	defer page.Close()
	if err := page.Goto(srv.URL); err != nil {
		t.Fatalf("goto: %v", err)
	}

	got, err := scrapeNotifications(page, n)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	return got
}

// Every cell has to come back. Reading this page with the post extractor would
// return the single reply and drop the other five, which is a worse answer than
// no answer: it looks complete.
func TestEveryNotificationCellIsRead(t *testing.T) {
	got := scrapeFixture(t, notificationFixture, 50)

	if len(got) != 5 {
		t.Fatalf("read %d notifications, want 5 (the article is not one): %+v", len(got), got)
	}
	for i, n := range got {
		if n.Text == "" {
			t.Errorf("notification %d has no text", i)
		}
		if n.CreatedAt.IsZero() {
			t.Errorf("notification %d has no timestamp", i)
		}
	}
}

// An aggregated cell keeps both actors rather than being split into rows X never
// rendered, and the post it concerns is reported apart from the words about it.
func TestAnAggregatedCellKeepsItsActors(t *testing.T) {
	got := scrapeFixture(t, notificationFixture, 50)

	var found bool
	for _, n := range got {
		if !strings.Contains(n.Text, "and Somnia Lab") {
			continue
		}
		found = true
		if len(n.Actors) != 2 {
			t.Errorf("actors %+v, want both accounts", n.Actors)
		}
		if n.PostText != "Reading X through the API is pay-per-use now." {
			t.Errorf("post text %q, want the post it concerns", n.PostText)
		}
		if strings.Contains(n.Text, "pay-per-use") {
			t.Error("the post text was run together with the words about it")
		}
	}
	if !found {
		t.Fatal("the two-actor cell was not read")
	}
}

// Kind is read from X's own words, so it is a convenience and not a contract.
// What matters is that it is right when it is set and absent when unsure.
func TestKindIsReadWhenTheWordsSayIt(t *testing.T) {
	got := scrapeFixture(t, notificationFixture, 50)

	kinds := map[string]string{}
	for _, n := range got {
		kinds[n.Text] = n.Kind
	}
	for text, kind := range kinds {
		var want string
		switch {
		case strings.Contains(text, "followed you"):
			want = model.NotifFollow
		case strings.Contains(text, "liked"):
			want = model.NotifLike
		case strings.Contains(text, "Recent post from"):
			want = model.NotifRecommended
		}
		if kind != want {
			t.Errorf("%q: kind %q, want %q", text, kind, want)
		}
	}
}

// Non-Latin text has to survive the round trip. This account's own notifications
// are largely Persian, so a fixture that only proves ASCII works proves little.
func TestNonLatinNotificationTextSurvives(t *testing.T) {
	got := scrapeFixture(t, notificationFixture, 50)

	for _, n := range got {
		if strings.Contains(n.Text, "liked 2 of your posts") {
			if !strings.Contains(n.Text, "پانیخذ") {
				t.Errorf("the Persian name did not survive: %q", n.Text)
			}
			return
		}
	}
	t.Fatal("the cell with a non-Latin name was not read")
}

// A follow names nobody's post, and must not borrow the next cell's.
func TestAFollowCarriesNoPostText(t *testing.T) {
	got := scrapeFixture(t, notificationFixture, 50)

	for _, n := range got {
		if strings.Contains(n.Text, "followed you") && n.PostText != "" {
			t.Errorf("a follow reported post text %q", n.PostText)
		}
	}
}

// The limit is honoured, since a caller asking for three should not be handed
// everything the page happened to render.
func TestTheNotificationLimitIsHonoured(t *testing.T) {
	if got := scrapeFixture(t, notificationFixture, 2); len(got) != 2 {
		t.Errorf("read %d, want 2", len(got))
	}
}
