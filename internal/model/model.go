// Package model holds the domain types shared across the server.
//
// It imports nothing from the rest of the project, so every other package can
// depend on it without creating a cycle.
package model

import (
	"sort"
	"strings"
	"time"
)

// Post is a single X post, normalized to the same shape regardless of whether
// it came from an API payload or was scraped out of the rendered page.
type Post struct {
	ID        string    `json:"id"`
	Text      string    `json:"text"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	Author    Author    `json:"author"`
	Metrics   Metrics   `json:"metrics"`
	// Media holds the images attached to the post. Plenty of posts carry no
	// text at all -- image-only replies are how threads of visual work are
	// usually published -- so this is often the entire content.
	Media []Media `json:"media,omitempty"`
}

// Media is an image attached to a post.
type Media struct {
	URL string `json:"url"`
	Alt string `json:"alt,omitempty"`
}

// Author is the account that published a post.
type Author struct {
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Handle string `json:"handle"`
}

// Metrics are the engagement counters displayed with a post. They are best
// effort: X abbreviates large numbers in the DOM, so values may be rounded.
type Metrics struct {
	Replies int `json:"replies"`
	Reposts int `json:"reposts"`
	Likes   int `json:"likes"`
	Views   int `json:"views,omitempty"`
}

// Thread is a root post together with the replies shown beneath it.
type Thread struct {
	Root    Post   `json:"root"`
	Replies []Post `json:"replies"`
}

// Contributor counts how many posts one account contributed to a result set.
type Contributor struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`
	Posts  int    `json:"posts"`
}

// Usable reports whether a post carries enough to be worth returning.
//
// Partially rendered timeline entries are common and are dropped rather than
// surfaced as blanks. Text is not required: an image-only post is complete,
// and requiring text silently discarded whole self-threads of visual work.
func (p Post) Usable() bool {
	if p.ID == "" || p.Author.Handle == "" {
		return false
	}
	return strings.TrimSpace(p.Text) != "" || len(p.Media) > 0
}

// Dedupe returns the usable posts in input order with duplicate IDs removed,
// capped at limit. A limit of zero or less means no cap.
//
// Timelines repeat posts across scroll rounds and merged sources, so callers
// accumulate through this rather than tracking seen IDs themselves.
func Dedupe(posts []Post, limit int) []Post {
	out := make([]Post, 0, len(posts))
	seen := make(map[string]struct{}, len(posts))

	for _, post := range posts {
		if !post.Usable() {
			continue
		}
		if _, dup := seen[post.ID]; dup {
			continue
		}
		seen[post.ID] = struct{}{}
		out = append(out, post)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// Contributors ranks the accounts in posts by how many they contributed, with
// ties broken by first appearance so the result is stable.
func Contributors(posts []Post) []Contributor {
	index := make(map[string]int, len(posts))
	out := make([]Contributor, 0, len(posts))

	for _, post := range posts {
		handle := post.Author.Handle
		if handle == "" {
			continue
		}
		if at, ok := index[handle]; ok {
			out[at].Posts++
			continue
		}
		index[handle] = len(out)
		out = append(out, Contributor{
			Handle: handle,
			Name:   post.Author.Name,
			Posts:  1,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Posts > out[j].Posts
	})
	return out
}

// Excerpt shortens text to at most maxRunes, appending an ellipsis when it had
// to cut. It counts runes rather than bytes so multi-byte posts are not split
// mid-character.
func Excerpt(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	if maxRunes <= 1 {
		return string(runes[:maxRunes])
	}
	return strings.TrimRight(string(runes[:maxRunes-1]), " ") + "…"
}

// Normalize collapses the whitespace X renders into post text so a post reads
// as a single line in summaries.
func Normalize(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
