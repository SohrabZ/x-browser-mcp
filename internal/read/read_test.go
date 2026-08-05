package read

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
	c := newCache(time.Minute)
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
	c := newCache(time.Minute)
	if _, ok := c.get("nope"); ok {
		t.Fatal("expected a miss")
	}
}

func TestCacheExpires(t *testing.T) {
	c := newCache(time.Millisecond)
	c.put("k", Result{})

	time.Sleep(5 * time.Millisecond)

	if _, ok := c.get("k"); ok {
		t.Fatal("expected the entry to have expired")
	}
}

// A zero TTL disables caching outright rather than caching forever, which is
// the dangerous reading of "no expiry".
func TestZeroTTLDisablesCaching(t *testing.T) {
	c := newCache(0)
	c.put("k", Result{})

	if _, ok := c.get("k"); ok {
		t.Fatal("a zero TTL must not cache")
	}
}

func TestInvalidateClearsEverything(t *testing.T) {
	c := newCache(time.Minute)
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
	c := newCache(time.Minute)
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
		got := cameBackEmpty(c.ctxErr, c.failed)

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
		"https://x.com/search",
		"https://x.com/i/lists/",
	} {
		_, _, err := r.FromURL(context.Background(), raw, 5)
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
