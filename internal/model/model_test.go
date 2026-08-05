package model

import (
	"reflect"
	"testing"
)

func post(id, handle, text string) Post {
	return Post{ID: id, Text: text, Author: Author{Handle: handle, Name: handle}}
}

func TestUsableRejectsPartialPosts(t *testing.T) {
	cases := map[string]Post{
		"no id":      {Text: "hi", Author: Author{Handle: "a"}},
		"no text":    {ID: "1", Author: Author{Handle: "a"}},
		"blank text": {ID: "1", Text: "   ", Author: Author{Handle: "a"}},
		"no author":  {ID: "1", Text: "hi"},
	}
	for name, p := range cases {
		if p.Usable() {
			t.Errorf("%s: expected post to be unusable", name)
		}
	}
	if !post("1", "a", "hi").Usable() {
		t.Error("expected a complete post to be usable")
	}
}

// Image-only posts are how visual self-threads are published. Requiring text
// silently dropped every one of them, so a ten-post thread read as empty.
func TestImageOnlyPostIsUsable(t *testing.T) {
	p := Post{
		ID:     "1",
		Author: Author{Handle: "a"},
		Media:  []Media{{URL: "https://pbs.twimg.com/media/abc.jpg"}},
	}

	if !p.Usable() {
		t.Fatal("a post carrying only an image is complete content")
	}
}

func TestPostWithNeitherTextNorMediaIsUnusable(t *testing.T) {
	p := Post{ID: "1", Author: Author{Handle: "a"}}

	if p.Usable() {
		t.Fatal("a post with no text and no media carries nothing")
	}
}

func TestDedupeKeepsFirstOccurrenceInOrder(t *testing.T) {
	got := Dedupe([]Post{
		post("1", "a", "one"),
		post("2", "b", "two"),
		post("1", "a", "one again"),
		post("3", "c", "three"),
	}, 0)

	want := []string{"1", "2", "3"}
	ids := make([]string, len(got))
	for i, p := range got {
		ids[i] = p.ID
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
}

func TestDedupeAppliesLimitAndDropsUnusable(t *testing.T) {
	got := Dedupe([]Post{
		post("1", "a", "one"),
		{ID: "2"}, // unusable, must not consume a slot
		post("3", "c", "three"),
		post("4", "d", "four"),
	}, 2)

	if len(got) != 2 {
		t.Fatalf("expected 2 posts, got %d", len(got))
	}
	if got[0].ID != "1" || got[1].ID != "3" {
		t.Fatalf("unexpected posts: %v, %v", got[0].ID, got[1].ID)
	}
}

func TestContributorsRankByCountThenFirstAppearance(t *testing.T) {
	got := Contributors([]Post{
		post("1", "alice", "a"),
		post("2", "bob", "b"),
		post("3", "bob", "c"),
		post("4", "carol", "d"),
	})

	if len(got) != 3 {
		t.Fatalf("expected 3 contributors, got %d", len(got))
	}
	if got[0].Handle != "bob" || got[0].Posts != 2 {
		t.Fatalf("expected bob with 2 posts first, got %+v", got[0])
	}
	// alice and carol both have one post; alice appeared first.
	if got[1].Handle != "alice" || got[2].Handle != "carol" {
		t.Fatalf("ties must keep first-appearance order, got %v then %v", got[1].Handle, got[2].Handle)
	}
}

func TestExcerptCountsRunesNotBytes(t *testing.T) {
	// Each of these is multi-byte; slicing by bytes would corrupt them.
	text := "سلام دنیا این یک تست است"
	got := Excerpt(text, 8)

	if runes := []rune(got); len(runes) > 8 {
		t.Fatalf("excerpt too long: %d runes", len(runes))
	}
	if !hasValidRunes(got) {
		t.Fatalf("excerpt split a multi-byte character: %q", got)
	}
}

func TestExcerptLeavesShortTextAlone(t *testing.T) {
	if got := Excerpt("short", 20); got != "short" {
		t.Fatalf("got %q, want %q", got, "short")
	}
}

func TestNormalizeCollapsesWhitespace(t *testing.T) {
	if got := Normalize("one\n\ttwo   three\n"); got != "one two three" {
		t.Fatalf("got %q", got)
	}
}

func hasValidRunes(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}
