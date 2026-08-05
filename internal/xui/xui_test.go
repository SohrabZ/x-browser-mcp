package xui

import (
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/proto"
)

func TestSearchURLEncodesQueryAndMode(t *testing.T) {
	got := SearchURL("model context protocol", Latest)

	if !strings.Contains(got, "q=model+context+protocol") {
		t.Errorf("query not encoded: %s", got)
	}
	if !strings.Contains(got, "f=live") {
		t.Errorf("latest mode must request the live tab: %s", got)
	}

	if top := SearchURL("go", Top); strings.Contains(top, "f=live") {
		t.Errorf("top mode must not request the live tab: %s", top)
	}
}

// A query is user input that lands in a URL; separators must not escape it.
func TestSearchURLEscapesSeparators(t *testing.T) {
	got := SearchURL("a&b=c #tag", Top)

	if strings.Contains(got, "a&b=c") {
		t.Fatalf("query separators must be escaped: %s", got)
	}
}

func TestNormalizeHandleStripsDecoration(t *testing.T) {
	cases := map[string]string{
		"@sohrab":              "sohrab",
		"  sohrab  ":           "sohrab",
		"https://x.com/sohrab": "sohrab",
		"x.com/sohrab":         "sohrab",
		"sohrab":               "sohrab",
	}
	for in, want := range cases {
		if got := NormalizeHandle(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestPostIDFromHref(t *testing.T) {
	cases := map[string]string{
		"/sohrab/status/123":         "123",
		"/sohrab/status/123/photo/1": "123",
		"/sohrab/status/123?s=20":    "123",
		"https://x.com/a/status/456": "456",
		"/sohrab":                    "",
		"":                           "",
	}
	for href, want := range cases {
		if got := PostIDFromHref(href); got != want {
			t.Errorf("%q: got %q, want %q", href, got, want)
		}
	}
}

func TestSignedInCookiesRequiresBothOnAnXDomain(t *testing.T) {
	x := func(name string) *proto.NetworkCookie {
		return &proto.NetworkCookie{Name: name, Domain: ".x.com"}
	}

	if SignedInCookies([]*proto.NetworkCookie{x("auth_token")}) {
		t.Error("auth_token alone is not a session")
	}
	if !SignedInCookies([]*proto.NetworkCookie{x("auth_token"), x("ct0")}) {
		t.Error("both cookies on x.com should count as signed in")
	}
}

// A cookie of the same name from an unrelated domain says nothing about the X
// session, and must not be mistaken for one.
func TestSignedInCookiesIgnoresOtherDomains(t *testing.T) {
	cookies := []*proto.NetworkCookie{
		{Name: "auth_token", Domain: "evil.example"},
		{Name: "ct0", Domain: "evil.example"},
	}
	if SignedInCookies(cookies) {
		t.Fatal("cookies from another domain must not count as an X session")
	}
}

func TestSignedInCookiesAcceptsLegacyTwitterDomain(t *testing.T) {
	cookies := []*proto.NetworkCookie{
		{Name: "auth_token", Domain: ".twitter.com"},
		{Name: "ct0", Domain: ".twitter.com"},
	}
	if !SignedInCookies(cookies) {
		t.Fatal("twitter.com cookies are still valid X sessions")
	}
}

func TestSignedInCookiesToleratesNils(t *testing.T) {
	if SignedInCookies([]*proto.NetworkCookie{nil, nil}) {
		t.Fatal("nil cookies must not panic or count")
	}
}

func TestOnLoginWall(t *testing.T) {
	if !OnLoginWall("https://x.com/i/flow/login?redirect_after_login=%2Fhome") {
		t.Error("expected the login flow to be recognised")
	}
	if OnLoginWall(HomeURL) {
		t.Error("the home page is not the login wall")
	}
}

func TestRawPostConvertsAndDropsIncomplete(t *testing.T) {
	raw := RawPost{
		Href:      "/sohrab/status/999",
		Text:      "hello\n  world",
		CreatedAt: "2026-08-04T12:00:00.000Z",
		Handle:    "@sohrab",
		Name:      "Sohrab",
		Likes:     5,
	}

	post, ok := raw.ToPost()
	if !ok {
		t.Fatal("expected a complete post to convert")
	}
	if post.ID != "999" {
		t.Errorf("id: got %q", post.ID)
	}
	if post.Text != "hello world" {
		t.Errorf("text should be whitespace-normalized, got %q", post.Text)
	}
	if post.Author.Handle != "sohrab" {
		t.Errorf("handle should be normalized, got %q", post.Author.Handle)
	}
	if post.URL != "https://x.com/sohrab/status/999" {
		t.Errorf("url: got %q", post.URL)
	}
	if post.CreatedAt.IsZero() {
		t.Error("timestamp should have parsed")
	}
}

func TestRawPostRejectsIncomplete(t *testing.T) {
	cases := map[string]RawPost{
		"no href":   {Text: "hi", Handle: "@a"},
		"no text":   {Href: "/a/status/1", Handle: "@a"},
		"no handle": {Href: "/a/status/1", Text: "hi"},
		"bad href":  {Href: "/a/profile", Text: "hi", Handle: "@a"},
	}
	for name, raw := range cases {
		if _, ok := raw.ToPost(); ok {
			t.Errorf("%s: expected the post to be dropped", name)
		}
	}
}

// A self-thread of images is a real and common shape: ten replies, none with
// any text. Requiring text discarded all of them and the thread read as empty.
func TestImageOnlyPostConverts(t *testing.T) {
	raw := RawPost{
		Href:   "/LogoDiffusion/status/2076415564449190235",
		Handle: "@LogoDiffusion",
		Media:  []RawMedia{{URL: "https://pbs.twimg.com/media/abc.jpg", Alt: "a logo"}},
	}

	post, ok := raw.ToPost()
	if !ok {
		t.Fatal("an image-only post must convert")
	}
	if len(post.Media) != 1 {
		t.Fatalf("expected 1 image, got %d", len(post.Media))
	}
	if post.Media[0].Alt != "a logo" {
		t.Errorf("alt text should be kept, got %q", post.Media[0].Alt)
	}
	if post.Text != "" {
		t.Errorf("expected empty text, got %q", post.Text)
	}
}

func TestPostWithNeitherTextNorMediaIsDropped(t *testing.T) {
	raw := RawPost{Href: "/a/status/1", Handle: "@a"}

	if _, ok := raw.ToPost(); ok {
		t.Fatal("a post with no text and no media carries nothing")
	}
}

func TestBlankMediaURLsAreIgnored(t *testing.T) {
	raw := RawPost{
		Href:   "/a/status/1",
		Text:   "hello",
		Handle: "@a",
		Media:  []RawMedia{{URL: "   "}, {URL: "https://pbs.twimg.com/media/x.jpg"}},
	}

	post, ok := raw.ToPost()
	if !ok {
		t.Fatal("expected the post to convert")
	}
	if len(post.Media) != 1 {
		t.Fatalf("blank URLs should be dropped, got %d entries", len(post.Media))
	}
}

func TestToPostsSkipsUnusableEntries(t *testing.T) {
	got := ToPosts([]RawPost{
		{Href: "/a/status/1", Text: "one", Handle: "@a"},
		{Href: "", Text: "broken", Handle: "@b"},
		{Href: "/c/status/3", Text: "three", Handle: "@c"},
	})

	if len(got) != 2 {
		t.Fatalf("expected 2 usable posts, got %d", len(got))
	}
}

// A bad timestamp should cost the timestamp, not the whole post.
func TestRawPostSurvivesUnparseableTimestamp(t *testing.T) {
	raw := RawPost{Href: "/a/status/1", Text: "hi", Handle: "@a", CreatedAt: "not-a-time"}

	post, ok := raw.ToPost()
	if !ok {
		t.Fatal("post should still convert")
	}
	if !post.CreatedAt.IsZero() {
		t.Error("expected the timestamp to be left zero")
	}
}

func TestSearchModeValidity(t *testing.T) {
	if !Latest.Valid() || !Top.Valid() {
		t.Error("latest and top are the supported modes")
	}
	if SearchMode("trending").Valid() {
		t.Error("unknown modes must be rejected")
	}
}

// X Articles are long-form posts. They render no tweetText at all -- the
// headline and body live under their own testids -- so a reader that only
// looked at tweetText returned an article as completely empty.
func TestArticleConvertsWithTitleAndBody(t *testing.T) {
	raw := RawPost{
		Href:   "/Alfred_Lin/status/2084636778791858256",
		Handle: "@Alfred_Lin",
		Title:  "Speed Above All Else",
		Text:   "If you read Heat Seeking Missile for Pain, you'll recall that...",
	}

	post, ok := raw.ToPost()
	if !ok {
		t.Fatal("an article must convert")
	}
	if post.Title != "Speed Above All Else" {
		t.Errorf("title: got %q", post.Title)
	}
	if post.Text == "" {
		t.Error("article body should be kept as the post text")
	}
}

// A title alone is enough to keep a post, even with no body or media yet.
func TestTitleAloneIsEnough(t *testing.T) {
	raw := RawPost{Href: "/a/status/1", Handle: "@a", Title: "Headline"}

	if _, ok := raw.ToPost(); !ok {
		t.Fatal("a titled article carries content even with no body")
	}
}
