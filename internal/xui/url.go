package xui

import (
	"fmt"
	"net/url"
	"strings"
)

// TargetKind is what an x.com URL points at.
type TargetKind string

const (
	TargetPost      TargetKind = "post"
	TargetProfile   TargetKind = "profile"
	TargetList      TargetKind = "list"
	TargetBookmarks TargetKind = "bookmarks"
	TargetHome      TargetKind = "home"
	TargetSearch    TargetKind = "search"

	// TargetMentions is the notifications tab that holds posts;
	// TargetNotifications is the "All" tab, most of which are not posts.
	TargetMentions      TargetKind = "mentions"
	TargetNotifications TargetKind = "notifications"
)

// Target is a parsed x.com URL.
type Target struct {
	Kind   TargetKind
	Handle string
	PostID string
	ListID string
	Query  string
}

// reservedPaths are x.com paths that are not account handles.
var reservedPaths = map[string]bool{
	"home": true, "explore": true, "notifications": true, "messages": true,
	"search": true, "settings": true, "compose": true, "i": true,
}

// ParseURL classifies an x.com URL.
//
// People paste links rather than assembling handle/id pairs, so this accepts
// what they actually copy: with or without a scheme, x.com or twitter.com, and
// with the tracking query X appends to its share links.
func ParseURL(raw string) (Target, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Target{}, fmt.Errorf("no URL given")
	}

	// A bare "x.com/user/status/1" has no scheme, and url.Parse would read the
	// whole thing as a path.
	if !strings.Contains(trimmed, "://") {
		trimmed = "https://" + strings.TrimPrefix(trimmed, "//")
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return Target{}, fmt.Errorf("not a URL: %q", raw)
	}
	if !isXHost(parsed.Host) {
		return Target{}, fmt.Errorf("not an x.com URL: %q", raw)
	}

	segments := splitPath(parsed.Path)
	if len(segments) == 0 {
		return Target{Kind: TargetHome}, nil
	}

	// /search?q=...
	if segments[0] == "search" {
		q := parsed.Query().Get("q")
		if q == "" {
			return Target{}, fmt.Errorf("search URL has no q parameter: %q", raw)
		}
		return Target{Kind: TargetSearch, Query: q}, nil
	}

	// /i/... covers X's internal routes: lists and bookmarks.
	if segments[0] == "i" {
		switch {
		case len(segments) >= 3 && segments[1] == "lists":
			return Target{Kind: TargetList, ListID: segments[2]}, nil
		case len(segments) >= 2 && segments[1] == "bookmarks":
			return Target{Kind: TargetBookmarks}, nil
		}
		return Target{}, fmt.Errorf("unsupported x.com URL: %q", raw)
	}

	if reservedPaths[segments[0]] {
		switch {
		case segments[0] == "home":
			return Target{Kind: TargetHome}, nil
		// /notifications is the "All" tab and /notifications/mentions the one
		// that holds posts. They read differently, so they are separate targets
		// rather than one with a flag.
		case segments[0] == "notifications" && len(segments) == 2 && segments[1] == "mentions":
			return Target{Kind: TargetMentions}, nil
		case segments[0] == "notifications" && len(segments) == 1:
			return Target{Kind: TargetNotifications}, nil
		}
		return Target{}, fmt.Errorf("unsupported x.com URL: %q", raw)
	}

	handle := NormalizeHandle(segments[0])
	if handle == "" {
		return Target{}, fmt.Errorf("no account in URL: %q", raw)
	}

	// /handle/status/123 -- also matches /statuses/123, X's older form.
	if len(segments) >= 3 && (segments[1] == "status" || segments[1] == "statuses") {
		id := segments[2]
		if id == "" {
			return Target{}, fmt.Errorf("no post id in URL: %q", raw)
		}
		return Target{Kind: TargetPost, Handle: handle, PostID: id}, nil
	}

	return Target{Kind: TargetProfile, Handle: handle}, nil
}

func isXHost(host string) bool {
	h := strings.ToLower(host)
	if i := strings.Index(h, ":"); i >= 0 {
		h = h[:i]
	}
	h = strings.TrimPrefix(h, "www.")
	h = strings.TrimPrefix(h, "mobile.")
	return h == "x.com" || h == "twitter.com"
}

func splitPath(path string) []string {
	out := make([]string, 0, 4)
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}
