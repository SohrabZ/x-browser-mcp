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
	ID string `json:"id"`
	// Title is set for X Articles, the long-form posts that carry a headline and
	// a body instead of the usual short text.
	Title     string    `json:"title,omitempty"`
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
	return strings.TrimSpace(p.Text) != "" || strings.TrimSpace(p.Title) != "" || len(p.Media) > 0
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

// Notification is one cell of X's notifications timeline.
//
// It is not a Post and does not reduce to one. X aggregates: a single cell can
// say two accounts liked one post, or that one account liked two of yours, and
// it renders no link to the post in either case. So a notification carries what
// the cell actually said -- who, when, and the words -- rather than a shape
// invented to look like a post.
type Notification struct {
	// Kind is best effort, and empty when it could not be told.
	//
	// X marks the difference between a like and a follow only with an icon that
	// carries no identifier, and with English words in the text, so this is read
	// from those words. It will be empty under a different interface language, and
	// it is empty for about a fifth of cells on a real account even in English:
	// X renders some as nothing but a display name and a time, with the post
	// underneath and no verb anywhere. For those, neither this nor Text says what
	// happened -- PostText is the only content, and a reader has to treat the cell
	// as "something involving this account and this post".
	Kind string `json:"kind,omitempty"`

	// Actors are the accounts the cell names. More than one is normal.
	Actors []Author `json:"actors,omitempty"`

	// Text is the line X wrote, such as "Ramyar Khalili and Somnia Lab liked
	// your post", including the relative time it renders after it. It is the only
	// field always present, though see Kind: for some cells it is only a name.
	Text string `json:"text"`

	// PostText is the post the notification concerns, when the cell shows one. A
	// follow shows none. There is no id or URL to give: the cell links to the
	// accounts involved and never to the post.
	PostText string `json:"post_text,omitempty"`

	CreatedAt time.Time `json:"created_at,omitempty"`
}

// Kinds a notification cell can be recognised as.
const (
	NotifLike        = "like"
	NotifRepost      = "repost"
	NotifFollow      = "follow"
	NotifMention     = "mention"
	NotifReply       = "reply"
	NotifRecommended = "recommended"
)

// Usable reports whether a notification says anything at all.
//
// Text is the test rather than Kind or Actors, because Text is what X wrote and
// the other two are read out of it. A cell with no words is a cell that failed
// to render.
func (n Notification) Usable() bool { return strings.TrimSpace(n.Text) != "" }

// DedupeNotifications drops repeats and caps the result.
//
// Scrolling re-reads cells that were already collected, and a notification has no
// id to key on, so identity has to be built from what the cell said.
//
// It is built from everything the cell said, not just the words and the second
// they arrived. Two likes from the same account on different posts within the
// same second render the same sentence, and a cell with no timestamp keys on the
// zero time -- so a coarser fingerprint drops real notifications and reports a
// shorter list as though the page held no more.
func DedupeNotifications(items []Notification, limit int) []Notification {
	if limit <= 0 {
		return nil
	}

	seen := make(map[string]bool, len(items))
	out := make([]Notification, 0, limit)
	for _, n := range items {
		if !n.Usable() {
			continue
		}
		key := n.fingerprint()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
		if len(out) == limit {
			break
		}
	}
	return out
}

// fingerprint is everything about a notification that a reader could tell apart.
//
// Nanoseconds rather than seconds, because X timestamps cells to the millisecond
// and two within one second are ordinary. Actors are sorted so the same pair in a
// different render order is still the same notification.
func (n Notification) fingerprint() string {
	handles := make([]string, 0, len(n.Actors))
	for _, a := range n.Actors {
		handles = append(handles, a.Handle)
	}
	sort.Strings(handles)

	return strings.Join([]string{
		n.CreatedAt.UTC().Format(time.RFC3339Nano),
		n.Kind,
		strings.Join(handles, ","),
		Normalize(n.Text),
		Normalize(n.PostText),
	}, "\x00")
}
