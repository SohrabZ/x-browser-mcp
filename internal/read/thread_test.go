package read

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/browser"
	"github.com/SohrabZ/x-browser-mcp/internal/model"
)

// Seed the collection boundary, not Thread's answer: the same ordered posts
// arrive here from a fresh DOM scrape or from the timeline cache.
func threadReader(posts []model.Post) *Reader {
	r := New(Options{CacheFor: time.Minute})
	r.cache.put(cacheKey("thread|target|200", 10), Result{Posts: posts})
	return r
}

func TestThreadSelectsRequestedPost(t *testing.T) {
	post := func(id string) model.Post {
		return model.Post{ID: id, Text: "post " + id, Author: model.Author{Handle: "someone"}}
	}
	for _, tc := range []struct {
		name    string
		posts   []model.Post
		replies []string
		missing bool
	}{
		{name: "top-level post", posts: []model.Post{post("200"), post("300")}, replies: []string{"300"}},
		{name: "ancestor before target", posts: []model.Post{post("100"), post("200"), post("300")}, replies: []string{"300"}},
		{name: "several ancestors without replies", posts: []model.Post{post("98"), post("99"), post("100"), post("200")}},
		{name: "target absent", posts: []model.Post{post("100"), post("300")}, missing: true},
		{name: "empty collection", missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := threadReader(tc.posts).Thread(t.Context(), "target", "200", 10)
			if tc.missing {
				var absent *NotFoundError
				if !errors.As(err, &absent) {
					t.Fatalf("missing requested post: got root %q, error %v; want NotFoundError", got.Root.ID, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Root.ID != "200" {
				t.Errorf("root = %q, want requested post 200", got.Root.ID)
			}
			var ids []string
			for _, p := range got.Replies {
				ids = append(ids, p.ID)
			}
			if !reflect.DeepEqual(ids, tc.replies) {
				t.Errorf("replies = %v, want %v (ancestors are not replies)", ids, tc.replies)
			}
		})
	}
}

// A real browser supplies the IDs and order, so the test also checks the
// extraction boundary. It uses synthetic HTML and a throwaway profile; no X
// account or network access to X is needed.
func TestThreadSelectsRequestedPostFromDOM(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a browser")
	}
	chrome := browser.ChromePathForTest()
	if chrome == "" {
		t.Skip("no Chrome installed")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		for _, p := range []struct{ handle, id, text string }{
			{"ancestor", "100", "Earlier conversation"},
			{"target", "200", "Requested post"},
			{"reply", "300", "A reply below it"},
		} {
			fmt.Fprintf(w, `<article data-testid="tweet">
<div data-testid="User-Name"><span>%s</span><span>@%s</span></div>
<a href="/%s/status/%s"><time datetime="2026-08-01T12:00:00Z">now</time></a>
<div data-testid="tweetText">%s</div></article>`, p.handle, p.handle, p.handle, p.id, p.text)
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	session, err := browser.Open(ctx, browser.Options{ChromePath: chrome, Headless: true})
	if err != nil {
		t.Fatalf("start fixture Chrome: %v", err)
	}
	defer session.Close()
	page, err := session.Page(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer page.Close()
	if err := page.Goto(srv.URL); err != nil {
		t.Fatal(err)
	}
	posts, err := scrape(page, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(posts) != 3 || posts[0].ID != "100" || posts[1].ID != "200" {
		t.Fatalf("unexpected fixture extraction: %+v", posts)
	}
	resolved, err := threadReader(posts).FromURL(ctx, "https://x.com/target/status/200?s=20", 10)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Thread == nil || resolved.Thread.Root.ID != "200" {
		t.Fatalf("URL resolved to the wrong root: %+v", resolved.Thread)
	}
	if len(resolved.Thread.Replies) != 1 || resolved.Thread.Replies[0].ID != "300" {
		t.Fatalf("wrong replies: %+v", resolved.Thread.Replies)
	}
}
