package read

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/model"
)

// postsFixture reads a page of posts through the real extraction script.
func postsFixture(t *testing.T, body string) []model.Post {
	t.Helper()
	return onFixture(t, body, scrape)
}

// quotePermalink is a quote post's own page as X served it while this was
// written: the quoted post is a link-role card with a byline, text, time and
// image of its own and no link to its status, and the post's own time sits at
// the bottom, below the card. So the first time in the article is the quoted
// post's, and the first image is too.
const quotePermalink = `
<article data-testid="tweet">
  <div data-testid="User-Name"><span>Quoter</span><span>@quoter</span></div>
  <div data-testid="tweetText">What the quoter said about it</div>
  <div role="link" tabindex="0">
    <div data-testid="User-Name"><span>Quoted</span><span>@quoted</span>
      <time datetime="2026-07-01T09:00:00.000Z">Jul 1</time></div>
    <div data-testid="tweetText">What the quoted account said</div>
    <div data-testid="tweetPhoto"><img src="https://pbs.twimg.com/media/quoted.jpg" alt="Image"></div>
  </div>
  <a href="/quoter/status/200" role="link"><time datetime="2026-08-01T12:00:00.000Z">12:00 PM</time></a>
  <a href="/quoter/status/200/analytics">Views</a>
  <div data-testid="like" aria-label="3 Likes. Like"></div>
</article>`

// A quote post is read from what it renders itself. Reading the article's whole
// subtree dated this post to when the quoted one was written and gave it the
// quoted post's image.
func TestAQuotePostKeepsItsOwnTimeAndImages(t *testing.T) {
	got := postsFixture(t, quotePermalink)
	if len(got) != 1 {
		t.Fatalf("read %d posts, want 1; the quoted card is not a post on the page: %+v", len(got), got)
	}
	p := got[0]

	if p.ID != "200" || p.Author.Handle != "quoter" {
		t.Errorf("read @%s/%s, want @quoter/200", p.Author.Handle, p.ID)
	}
	if p.Text != "What the quoter said about it" {
		t.Errorf("text = %q, want the quoter's own words", p.Text)
	}
	if want := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC); !p.CreatedAt.Equal(want) {
		t.Errorf("created_at = %s, want %s; the quoted post's time was taken as this post's", p.CreatedAt, want)
	}
	if len(p.Media) != 0 {
		t.Errorf("the quoted post's image was attributed to the quoter: %+v", p.Media)
	}
	if p.Metrics.Likes != 3 {
		t.Errorf("likes = %d, want the post's own 3", p.Metrics.Likes)
	}
}

// And what the card says is kept, attributed to its own author, since a quote's
// words are often meaningless without the post they are about.
func TestAQuotedPostIsKeptApartFromTheQuote(t *testing.T) {
	got := postsFixture(t, quotePermalink)
	if len(got) != 1 || got[0].Quoted == nil {
		t.Fatalf("no quoted post read: %+v", got)
	}
	q := got[0].Quoted

	if q.Author.Handle != "quoted" || q.Text != "What the quoted account said" {
		t.Errorf("quoted = @%s %q, want @quoted and its own words", q.Author.Handle, q.Text)
	}
	if len(q.Media) != 1 {
		t.Errorf("quoted media = %+v, want its one image", q.Media)
	}
	if want := time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC); !q.CreatedAt.Equal(want) {
		t.Errorf("quoted created_at = %s, want %s", q.CreatedAt, want)
	}
	// The card does not link to the quoted post, and borrowing the link around
	// the quoter's time would give the quote the quoter's id.
	if q.ID != "" || q.URL != "" {
		t.Errorf("quoted id = %q, url = %q; the card has no link of its own", q.ID, q.URL)
	}
}

// A quote that adds no words of its own used to read as the quoted account's
// words under the quoter's name. It is still a post, and says what it quotes.
func TestAQuoteWithNoWordsOfItsOwnDoesNotBorrowThem(t *testing.T) {
	got := postsFixture(t, `
<article data-testid="tweet">
  <div data-testid="User-Name"><span>Quoter</span><span>@quoter</span>
    <a href="/quoter/status/201"><time datetime="2026-08-01T12:00:00.000Z">1h</time></a></div>
  <div role="link" tabindex="0">
    <div data-testid="User-Name"><span>Quoted</span><span>@quoted</span></div>
    <div data-testid="tweetText">Words only the quoted account wrote</div>
  </div>
</article>`)
	if len(got) != 1 {
		t.Fatalf("read %d posts, want the quote kept: %+v", len(got), got)
	}
	if got[0].Text != "" {
		t.Errorf("the quoter was given the quoted account's words: %q", got[0].Text)
	}
	if got[0].Quoted == nil || got[0].Quoted.Text != "Words only the quoted account wrote" {
		t.Errorf("quoted = %+v, want the quoted words there instead", got[0].Quoted)
	}
}

// A quoted post nested as an article of its own is excluded the same way, and is
// not a separate post either. Its link, when it has one, identifies it.
func TestANestedQuotedArticleIsNotAPostOfItsOwn(t *testing.T) {
	got := postsFixture(t, `
<article data-testid="tweet">
  <div data-testid="User-Name"><span>Quoter</span><span>@quoter</span></div>
  <a href="/quoter/status/202"><time datetime="2026-08-01T12:00:00.000Z">1h</time></a>
  <div data-testid="tweetText">Look at this</div>
  <div><article data-testid="tweet">
    <div data-testid="User-Name"><span>Quoted</span><span>@quoted</span></div>
    <a href="/quoted/status/555"><time datetime="2026-07-01T09:00:00.000Z">Jul 1</time></a>
    <div data-testid="tweetText">The quoted post</div>
    <div data-testid="like" aria-label="9000 Likes"></div>
  </article></div>
  <div data-testid="like" aria-label="3 Likes"></div>
</article>`)
	if len(got) != 1 {
		t.Fatalf("read %d posts, want 1: %+v", len(got), got)
	}
	if got[0].Metrics.Likes != 3 {
		t.Errorf("likes = %d, want the post's own 3 rather than the quoted post's", got[0].Metrics.Likes)
	}
	if q := got[0].Quoted; q == nil || q.ID != "555" || q.URL != "https://x.com/quoted/status/555" {
		t.Errorf("quoted = %+v, want post 555 with its address", q)
	}
}

// An X Article renders its headline, body and cover inside an article element of
// its own, as X served one while this was written. All of it is the post's: it
// is only a nested post article that belongs to somebody else.
func TestAnXArticlesBodyIsItsOwn(t *testing.T) {
	got := postsFixture(t, `
<article data-testid="tweet">
  <div data-testid="User-Name"><span>Alfred Lin</span><span>@Alfred_Lin</span></div>
  <article data-testid="twitterArticleReadView" role="article">
    <a role="link" href="/Alfred_Lin/article/300/media/1"><div data-testid="tweetPhoto">
      <img src="https://pbs.twimg.com/media/cover.jpg" alt="Image"></div></a>
    <div data-testid="twitter-article-title">The headline</div>
    <div data-testid="twitterArticleRichTextView"><div data-testid="longformRichTextComponent">The body, at length.</div></div>
  </article>
  <a href="/Alfred_Lin/status/300" role="link"><time datetime="2026-08-01T12:00:00.000Z">12:00 PM</time></a>
  <div data-testid="like" aria-label="2517 Likes. Like"></div>
</article>`)
	if len(got) != 1 {
		t.Fatalf("read %d posts, want the article: %+v", len(got), got)
	}
	p := got[0]
	if p.ID != "300" || p.Title != "The headline" || p.Text != "The body, at length." || len(p.Media) != 1 {
		t.Errorf("read %+v, want post 300 with its headline, body and cover", p)
	}
	if p.Quoted != nil {
		t.Errorf("the article's own body was read as a quote: %+v", p.Quoted)
	}
}

// Capping in the page capped elements before the ones still rendering were
// discarded, so articles that had not filled in yet hid the posts below them.
func TestArticlesStillRenderingDoNotHideThePostsBelow(t *testing.T) {
	body := strings.Repeat(`<article data-testid="tweet"></article>`, 8)
	for _, id := range []string{"101", "102", "103", "104", "105"} {
		body += `<article data-testid="tweet">
  <div data-testid="User-Name"><span>Someone</span><span>@someone</span></div>
  <a href="/someone/status/` + id + `"><time datetime="2026-08-01T12:00:00.000Z">1h</time></a>
  <div data-testid="tweetText">post ` + id + `</div></article>`
	}

	if got := postsFixture(t, body); len(got) != 5 {
		t.Errorf("read %d posts, want all 5 below the empty articles", len(got))
	}
}

// A reply's permalink as X renders it: the post it answers above it, and a reply
// below. Read through the real script, so the order is the page's.
func TestAReplyIsReadAsTheRootWithItsContextAbove(t *testing.T) {
	article := func(handle, id, text string) string {
		return `<article data-testid="tweet">
  <div data-testid="User-Name"><span>` + handle + `</span><span>@` + handle + `</span>
    <a href="/` + handle + `/status/` + id + `"><time datetime="2026-08-01T12:00:00.000Z">1h</time></a></div>
  <div data-testid="tweetText">` + text + `</div></article>`
	}
	posts := postsFixture(t, article("ancestor", "100", "The post being answered")+
		article("target", "200", "The reply that was asked for")+
		article("reply", "300", "A reply to the reply"))

	thread, found := threadFrom(posts, "200", 8)
	if !found {
		t.Fatalf("post 200 not found among %+v", posts)
	}
	if thread.Root.ID != "200" {
		t.Errorf("root = %s, want the post asked for, not the top of the page", thread.Root.ID)
	}
	if got := ids(thread.Ancestors); !reflect.DeepEqual(got, []string{"100"}) {
		t.Errorf("ancestors = %v, want [100]", got)
	}
	if got := ids(thread.Replies); !reflect.DeepEqual(got, []string{"300"}) {
		t.Errorf("replies = %v, want [300]", got)
	}
}

func TestThreadFromPlacesThePostItWasAskedFor(t *testing.T) {
	post := func(id string) model.Post {
		return model.Post{ID: id, Text: "post " + id, Author: model.Author{Handle: "someone"}}
	}
	posts := func(ids ...string) []model.Post {
		out := make([]model.Post, 0, len(ids))
		for _, id := range ids {
			out = append(out, post(id))
		}
		return out
	}

	for _, c := range []struct {
		name      string
		posts     []model.Post
		n         int
		ancestors []string
		replies   []string
		missing   bool
	}{
		{name: "top-level post", posts: posts("200", "300"), n: 8, replies: []string{"300"}},
		{name: "one ancestor", posts: posts("100", "200", "300"), n: 8, ancestors: []string{"100"}, replies: []string{"300"}},
		{name: "several ancestors, no replies", posts: posts("98", "99", "100", "200"), n: 8, ancestors: []string{"98", "99", "100"}},
		{name: "replies capped, ancestors not", posts: posts("100", "200", "301", "302", "303"), n: 2, ancestors: []string{"100"}, replies: []string{"301", "302"}},
		{name: "post absent", posts: posts("100", "300"), n: 8, missing: true},
		{name: "nothing read", n: 8, missing: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, found := threadFrom(c.posts, "200", c.n)
			if found == c.missing {
				t.Fatalf("found = %v, want %v", found, !c.missing)
			}
			if c.missing {
				return
			}
			if got.Root.ID != "200" {
				t.Errorf("root = %q, want 200", got.Root.ID)
			}
			if a := ids(got.Ancestors); !reflect.DeepEqual(a, c.ancestors) {
				t.Errorf("ancestors = %v, want %v", a, c.ancestors)
			}
			if r := ids(got.Replies); !reflect.DeepEqual(r, c.replies) {
				t.Errorf("replies = %v, want %v", r, c.replies)
			}
		})
	}
}

// Collection stops once it has the post and n replies, and not before. Counting
// n posts from the top of the page spent the limit on the posts above it, and
// could stop before reaching the post at all.
func TestAThreadIsCompleteOnlyWithThePostAndItsReplies(t *testing.T) {
	post := func(id string) model.Post { return model.Post{ID: id} }
	enough := threadComplete("200", 2)

	for _, c := range []struct {
		name  string
		posts []model.Post
		want  bool
	}{
		{"ancestors alone, however many", []model.Post{post("1"), post("2"), post("3"), post("4")}, false},
		{"the post, one reply short", []model.Post{post("1"), post("200"), post("301")}, false},
		{"the post and two replies", []model.Post{post("1"), post("200"), post("301"), post("302")}, true},
	} {
		if got := enough(c.posts); got != c.want {
			t.Errorf("%s: complete = %v, want %v", c.name, got, c.want)
		}
	}
}

// An id that is not digits can never match a status link, so it would load the
// right page and then report the post missing from it. It is the caller's
// mistake, and is said to be before anything is launched.
func TestAThreadIDThatIsNotDigitsIsTheCallersMistake(t *testing.T) {
	r := New(Options{})
	for _, id := range []string{"", "200?s=20", "200/", "abc"} {
		_, err := r.Thread(context.Background(), "someone", id, 5)
		var bad *InvalidError
		if !errors.As(err, &bad) {
			t.Errorf("id %q: got %v, want an InvalidError", id, err)
		}
	}
}

func ids(posts []model.Post) []string {
	var out []string
	for _, p := range posts {
		out = append(out, p.ID)
	}
	return out
}
