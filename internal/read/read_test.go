package read

import (
	"testing"
	"time"

	"x-browser-mcp/internal/model"
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
