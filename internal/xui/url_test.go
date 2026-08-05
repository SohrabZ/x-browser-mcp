package xui

import "testing"

// The URL people actually paste: X's share links carry a ?s= tracking param,
// and the whole point is that a caller should not have to strip it or split the
// handle out by hand.
func TestParseSharedPostURL(t *testing.T) {
	got, err := ParseURL("x.com/LogoDiffusion/status/2076415564449190234?s=20")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got.Kind != TargetPost {
		t.Errorf("kind: got %q, want %q", got.Kind, TargetPost)
	}
	if got.Handle != "LogoDiffusion" {
		t.Errorf("handle: got %q", got.Handle)
	}
	if got.PostID != "2076415564449190234" {
		t.Errorf("post id: got %q", got.PostID)
	}
}

func TestParsePostURLVariants(t *testing.T) {
	cases := []string{
		"https://x.com/user/status/123",
		"http://x.com/user/status/123",
		"x.com/user/status/123",
		"//x.com/user/status/123",
		"https://www.x.com/user/status/123",
		"https://mobile.x.com/user/status/123",
		"https://twitter.com/user/status/123",
		"https://x.com/user/status/123/photo/1",
		"https://x.com/user/statuses/123",
		"  https://x.com/user/status/123  ",
	}
	for _, raw := range cases {
		got, err := ParseURL(raw)
		if err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if got.Kind != TargetPost || got.Handle != "user" || got.PostID != "123" {
			t.Errorf("%q: got %+v", raw, got)
		}
	}
}

func TestParseProfileURL(t *testing.T) {
	got, err := ParseURL("https://x.com/golang")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Kind != TargetProfile || got.Handle != "golang" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseListURL(t *testing.T) {
	got, err := ParseURL("https://x.com/i/lists/1234567890")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Kind != TargetList || got.ListID != "1234567890" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseBookmarksAndHome(t *testing.T) {
	bookmarks, err := ParseURL("https://x.com/i/bookmarks")
	if err != nil || bookmarks.Kind != TargetBookmarks {
		t.Errorf("bookmarks: got %+v, err %v", bookmarks, err)
	}

	for _, raw := range []string{"https://x.com/home", "https://x.com/", "https://x.com"} {
		home, err := ParseURL(raw)
		if err != nil || home.Kind != TargetHome {
			t.Errorf("%q: got %+v, err %v", raw, home, err)
		}
	}
}

func TestParseSearchURL(t *testing.T) {
	got, err := ParseURL("https://x.com/search?q=model+context+protocol&f=live")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Kind != TargetSearch {
		t.Fatalf("kind: got %q", got.Kind)
	}
	if got.Query != "model context protocol" {
		t.Fatalf("query: got %q", got.Query)
	}
}

// Reserved paths are X's own routes, not accounts. Treating /messages as a
// profile would send a reader to a page that has no such account.
func TestReservedPathsAreNotTreatedAsProfiles(t *testing.T) {
	for _, raw := range []string{
		"https://x.com/messages",
		"https://x.com/settings",
		"https://x.com/explore",
		"https://x.com/i/unknown",
	} {
		if got, err := ParseURL(raw); err == nil {
			t.Errorf("%q should be unsupported, got %+v", raw, got)
		}
	}
}

// The two notification routes are reserved paths that this does read, and they
// read differently: one holds posts and the other mostly does not.
func TestTheNotificationRoutesAreDistinctTargets(t *testing.T) {
	cases := map[string]TargetKind{
		"https://x.com/notifications":          TargetNotifications,
		"https://x.com/notifications/mentions": TargetMentions,
		"https://twitter.com/notifications":    TargetNotifications,
		"x.com/notifications/mentions":         TargetMentions,
	}
	for raw, want := range cases {
		got, err := ParseURL(raw)
		if err != nil {
			t.Errorf("%q: %v", raw, err)
			continue
		}
		if got.Kind != want {
			t.Errorf("%q: kind %q, want %q", raw, got.Kind, want)
		}
	}

	// A tab this cannot read is still unsupported rather than guessed at, and
	// neither is anything hanging off the one it can: /mentions is the route, so
	// /mentions/something is not it.
	for _, raw := range []string{
		"https://x.com/notifications/verified",
		"https://x.com/notifications/mentions/extra",
		"https://x.com/notifications/mentions/1/2",
	} {
		if got, err := ParseURL(raw); err == nil {
			t.Errorf("%q was accepted as %+v", raw, got)
		}
	}
}

func TestParseRejectsNonXURLs(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"https://example.com/user/status/1",
		"https://notx.com/a",
		"https://x.com.evil.example/user/status/1",
	} {
		if _, err := ParseURL(raw); err == nil {
			t.Errorf("%q should have been rejected", raw)
		}
	}
}

func TestParseRejectsSearchWithoutQuery(t *testing.T) {
	if _, err := ParseURL("https://x.com/search"); err == nil {
		t.Fatal("a search URL with no q should be rejected")
	}
}
