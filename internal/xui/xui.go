// Package xui holds everything that knows what X's pages look like: URLs,
// selectors, and the scripts that pull posts out of the rendered DOM.
//
// Isolating it here means the selectors X breaks every few months are all in
// one place, and the parsing half can be tested against recorded HTML with no
// browser present.
package xui

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod/lib/proto"

	"github.com/SohrabZ/x-browser-mcp/internal/model"
)

// Page URLs.
const (
	HomeURL      = "https://x.com/home"
	BookmarksURL = "https://x.com/i/bookmarks"

	// NotificationsURL is the "All" tab: likes, follows, reposts and X's own
	// recommendations, most of which are not posts. MentionsURL is the tab that
	// is, so it reads on the ordinary post path.
	NotificationsURL = "https://x.com/notifications"
	MentionsURL      = "https://x.com/notifications/mentions"
	loginPath        = "/i/flow/login"
)

// SearchMode selects which tab of X search results to read.
type SearchMode string

const (
	// Latest is reverse-chronological; Top is X's relevance ranking.
	Latest SearchMode = "latest"
	Top    SearchMode = "top"
)

// Valid reports whether the mode is one X understands.
func (m SearchMode) Valid() bool {
	return m == Latest || m == Top
}

// SearchURL builds a search results URL for the query.
func SearchURL(query string, mode SearchMode) string {
	v := url.Values{}
	v.Set("q", query)
	v.Set("src", "typed_query")
	if mode == Latest {
		v.Set("f", "live")
	}
	return "https://x.com/search?" + v.Encode()
}

// UserURL is an account's posts timeline.
func UserURL(handle string) string {
	return "https://x.com/" + url.PathEscape(NormalizeHandle(handle))
}

// PostURL is the canonical permalink for a post.
func PostURL(handle, postID string) string {
	return fmt.Sprintf("https://x.com/%s/status/%s", NormalizeHandle(handle), postID)
}

// ListURL is a curated list's timeline.
func ListURL(listID string) string {
	return "https://x.com/i/lists/" + url.PathEscape(listID)
}

// NormalizeHandle strips the decoration users type around an @handle.
func NormalizeHandle(handle string) string {
	h := strings.TrimSpace(handle)
	h = strings.TrimPrefix(h, "@")
	if i := strings.LastIndex(h, "/"); i >= 0 {
		h = h[i+1:]
	}
	return h
}

// PostIDFromHref extracts the numeric post ID from a /status/ link.
func PostIDFromHref(href string) string {
	parts := strings.Split(strings.TrimSpace(href), "/")
	for i := 0; i < len(parts)-1; i++ {
		if parts[i] == "status" {
			id := parts[i+1]
			// Trim any /photo/1 or query suffix that follows the ID.
			if j := strings.IndexAny(id, "?#"); j >= 0 {
				id = id[:j]
			}
			return id
		}
	}
	return ""
}

// Selectors X renders. Grouped so a breakage has one place to be fixed.
const (
	// SelAccountMenu and SelProfileLink only exist for a signed-in viewer.
	SelAccountMenu = `[data-testid="SideNav_AccountSwitcher_Button"]`
	SelProfileLink = `[data-testid="AppTabBar_Profile_Link"]`

	SelComposeBox     = `[data-testid="tweetTextarea_0"]`
	SelComposeButton  = `[data-testid="tweetButtonInline"], [data-testid="tweetButton"]`
	SelReplyButton    = `[data-testid="reply"]`
	SelLikeButton     = `[data-testid="like"]`
	SelUnlikeButton   = `[data-testid="unlike"]`
	SelRepostButton   = `[data-testid="retweet"]`
	SelRepostConfirm  = `[data-testid="retweetConfirm"]`
	SelUnrepostButton = `[data-testid="unretweet"]`
	SelBookmarkAdd    = `[data-testid="bookmark"]`
	SelBookmarkRemove = `[data-testid="removeBookmark"]`

	// SelPost is the article X wraps around a single post, and the unit the
	// engagement controls above belong to. See ControlScript.
	SelPost = `article[data-testid="tweet"]`

	// SelNotification is a cell of the notifications timeline. It is not a post:
	// see NotificationScript.
	SelNotification = `[data-testid="notification"]`
)

// RawNotification is a notification cell as scraped, before validation. It
// matches the shape returned by NotificationScript.
type RawNotification struct {
	Kind    string   `json:"kind"`
	Handles []string `json:"handles"`
	// Display names are not collected per account: the cell names them in its
	// text but does not attach them to the avatars in any way that survives
	// aggregation. Text carries them; Actors carry the handles.
	Text      string `json:"text"`
	PostText  string `json:"post_text"`
	CreatedAt string `json:"created_at"`
}

// NotificationScript reads every notification cell currently rendered.
//
// It takes no limit and applies none. Capping here would cap DOM nodes before
// anything has been deduplicated or discarded, so a page whose first cells repeat
// would return fewer than asked for while the rest sat rendered just below. The
// cap belongs after conversion, where what is counted is notifications rather
// than elements.
//
// This exists because the notifications timeline is not a timeline of posts. Of
// eighteen cells on a real account, one was a post and sixteen were cells with
// no post in them at all -- likes, follows, and X's own "recent post from"
// recommendations. Running the post extractor over this page returns the one and
// silently drops the rest, which is a worse answer than none.
//
// What a cell reliably gives is a timestamp, the accounts it names, and the words
// X wrote. What it does not give is a link to the post it concerns -- every link
// in the cell goes to an account -- so there is no id or URL to report, only the
// post's text where the cell shows it.
//
// Kind is read from those words and is empty when none match. X distinguishes a
// like from a follow with an icon carrying no identifier, so there is nothing
// language-independent to read; Text still says what happened.
const NotificationScript = `() => {
  const kindOf = text => {
    const t = text.toLowerCase();
    if (t.includes('followed you')) return 'follow';
    if (t.includes('liked')) return 'like';
    if (t.includes('reposted')) return 'repost';
    if (t.includes('replying to') || t.includes('replied')) return 'reply';
    if (t.includes('mentioned you')) return 'mention';
    if (t.startsWith('recent post from') || t.includes('there was a post')) return 'recommended';
    return '';
  };

  return Array.from(document.querySelectorAll('[data-testid="notification"]'))
    .map(cell => {
      // Everything is read from this cell and nowhere else. Widening to the
      // surrounding wrapper when a cell holds nothing looks harmless and is not:
      // a follow has no post, so the wrapper hands it the neighbouring cell's,
      // and the notification then reports a post it has nothing to do with.
      // Absent has to stay absent -- there is no way to tell "X moved this out of
      // the cell" from "this cell does not have one", and guessing wrong invents
      // content rather than losing it.
      //
      // A quoted post is excluded the same way ControlScript excludes one: by
      // ownership. Anything inside a nested article belongs to that post, not to
      // this notification, so its text and its author are not this cell's.
      const within = (sel) => Array.from(cell.querySelectorAll(sel))
        .filter(e => e.closest('article') === cell.closest('article'));

      const post = within('[data-testid="tweetText"]')[0] || null;

      // The words X wrote, without the post it quotes underneath -- that is
      // reported separately rather than run together with the event.
      //
      // Taken off the end, not replaced: X renders the post beneath the event, and
      // the post's words can also occur in the event itself. "Alice liked your
      // post" over a post reading "your post" would otherwise lose the phrase from
      // the sentence and keep it in the quote.
      const postText = post ? post.innerText : '';
      let text = cell.innerText || '';
      if (postText) {
        const at = text.lastIndexOf(postText);
        if (at !== -1) text = text.slice(0, at) + text.slice(at + postText.length);
      }

      const handles = within('[data-testid^="UserAvatar-Container-"]')
        .map(e => e.getAttribute('data-testid').replace('UserAvatar-Container-', ''))
        .filter(Boolean);

      return {
        kind: kindOf(text),
        handles: Array.from(new Set(handles)),
        text: text,
        post_text: postText,
        created_at: (within('time')[0] || {}).dateTime || '',
      };
    });
}`

// ControlScript finds one post's engagement control and, when asked to, presses
// it. It takes a post id, a selector and a flag, and returns "ok", "no-control"
// if the post is there without the control, or "no-post" if X has not rendered
// any post yet.
//
// A permalink page is not one post. X renders the post's ancestors above it, its
// replies below, and any quoted post inside it, and gives every one of them its
// own reply, repost, like and bookmark row. A selector matched against the
// document therefore finds whichever of those comes first, so one reply the
// viewer had already liked was enough for a like to report success without ever
// touching the post that was asked for.
//
// The post wanted is the article that links to its own status id. X leaves that
// link off the post you are already looking at, so an article claiming the id is
// preferred and the first article is the fallback -- which is the post itself on
// a top-level permalink, and is what an unscoped selector would have found.
//
// The press is a DOM click rather than a synthesized mouse event, because X
// ignores those on these controls: the page accepts the event, reports nothing
// wrong, and makes no request.
//
// Only what a post renders itself counts, for both halves of that. X nests a
// quoted post's article inside the quoting one, so a plain subtree search
// reaches links and controls belonging to a different post -- which would let a
// quoted post's status link decide the match, and let its like button be the one
// pressed.
const ControlScript = `(postID, selector, press) => {
  const posts = Array.from(document.querySelectorAll('article[data-testid="tweet"]'));
  if (posts.length === 0) return 'no-post';

  const own = (post, sel) =>
    Array.from(post.querySelectorAll(sel)).filter(el => el.closest('article') === post);

  const owns = post => own(post, 'a[href*="/status/"]').some(link => {
    const m = /\/status\/(\d+)/.exec(link.getAttribute('href') || '');
    return m !== null && m[1] === postID;
  });

  const controls = own(posts.find(owns) || posts[0], selector);
  if (controls.length === 0) return 'no-control';
  if (press) controls[0].click();
  return 'ok';
}`

// ValidPostID reports whether id has the shape X gives a post: digits, and
// nothing else.
//
// Everything downstream leans on that. It goes into a URL unescaped, and
// ControlScript matches it against the digits in a status link -- which anything
// else can never match, so an id of the wrong shape would quietly fall back to
// the first post on the page rather than fail.
func ValidPostID(id string) bool {
	if id == "" || len(id) > maxPostIDDigits {
		return false
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// maxPostIDDigits is well clear of the 19 digits a 64-bit snowflake id needs.
const maxPostIDDigits = 25

// SignedInCookies reports whether the browser holds the pair of cookies X sets
// for an authenticated session.
//
// Only x.com cookies count: a cookie of the same name from another domain says
// nothing about this session.
func SignedInCookies(cookies []*proto.NetworkCookie) bool {
	var auth, csrf bool
	for _, c := range cookies {
		if c == nil || !isXDomain(c.Domain) {
			continue
		}
		switch c.Name {
		case "auth_token":
			auth = true
		case "ct0":
			csrf = true
		}
	}
	return auth && csrf
}

func isXDomain(domain string) bool {
	d := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(domain)), ".")
	return d == "x.com" || strings.HasSuffix(d, ".x.com") ||
		d == "twitter.com" || strings.HasSuffix(d, ".twitter.com")
}

// OnLoginWall reports whether a URL is X's signed-out login flow.
func OnLoginWall(currentURL string) bool {
	return strings.Contains(currentURL, loginPath)
}

// RawPost is a post as scraped from the DOM, before validation. It matches the
// shape returned by ExtractScript.
type RawPost struct {
	Href      string     `json:"href"`
	Title     string     `json:"title"`
	Text      string     `json:"text"`
	CreatedAt string     `json:"created_at"`
	Handle    string     `json:"handle"`
	Name      string     `json:"name"`
	Replies   int        `json:"replies"`
	Reposts   int        `json:"reposts"`
	Likes     int        `json:"likes"`
	Media     []RawMedia `json:"media"`
}

// RawMedia is an image as scraped from the DOM.
type RawMedia struct {
	URL string `json:"url"`
	Alt string `json:"alt"`
}

// ToPost converts a scraped post into a domain post, reporting whether it was
// complete enough to keep.
func (r RawPost) ToPost() (model.Post, bool) {
	handle := NormalizeHandle(r.Handle)
	id := PostIDFromHref(r.Href)
	text := model.Normalize(r.Text)

	media := make([]model.Media, 0, len(r.Media))
	for _, m := range r.Media {
		if url := strings.TrimSpace(m.URL); url != "" {
			media = append(media, model.Media{URL: url, Alt: strings.TrimSpace(m.Alt)})
		}
	}

	title := model.Normalize(r.Title)

	// Text is deliberately not required. Image-only posts are how visual
	// self-threads are published, and an X Article carries a title and body
	// rather than tweet text; demanding text dropped both entirely.
	if handle == "" || id == "" || (text == "" && title == "" && len(media) == 0) {
		return model.Post{}, false
	}

	post := model.Post{
		ID:     id,
		Title:  title,
		Text:   text,
		URL:    PostURL(handle, id),
		Author: model.Author{Name: strings.TrimSpace(r.Name), Handle: handle},
		Metrics: model.Metrics{
			Replies: r.Replies,
			Reposts: r.Reposts,
			Likes:   r.Likes,
		},
	}
	if len(media) > 0 {
		post.Media = media
	}
	if r.CreatedAt != "" {
		if at, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil {
			post.CreatedAt = at.UTC()
		}
	}
	return post, true
}

// ToPosts converts scraped posts, dropping incomplete ones.
func ToPosts(raw []RawPost) []model.Post {
	out := make([]model.Post, 0, len(raw))
	for _, r := range raw {
		if post, ok := r.ToPost(); ok {
			out = append(out, post)
		}
	}
	return out
}

// ToNotification converts a scraped cell, reporting whether it said enough to
// keep.
func (r RawNotification) ToNotification() (model.Notification, bool) {
	text := model.Normalize(r.Text)
	if text == "" {
		return model.Notification{}, false
	}

	actors := make([]model.Author, 0, len(r.Handles))
	for _, h := range r.Handles {
		if handle := NormalizeHandle(h); handle != "" {
			actors = append(actors, model.Author{Handle: handle})
		}
	}

	n := model.Notification{
		Kind:     r.Kind,
		Text:     text,
		PostText: model.Normalize(r.PostText),
	}
	if len(actors) > 0 {
		n.Actors = actors
	}
	if at, err := time.Parse(time.RFC3339, r.CreatedAt); err == nil {
		n.CreatedAt = at.UTC()
	}
	return n, true
}

// ToNotifications converts scraped cells, dropping the ones that said nothing.
func ToNotifications(raw []RawNotification) []model.Notification {
	out := make([]model.Notification, 0, len(raw))
	for _, r := range raw {
		if n, ok := r.ToNotification(); ok {
			out = append(out, n)
		}
	}
	return out
}

// ExtractScript reads the posts currently rendered on the page.
//
// It takes a limit and returns RawPost-shaped objects. Metric labels are
// abbreviated by X ("1.2K"), so the counts it returns are approximate.
const ExtractScript = `limit => {
  const count = label => {
    if (!label) return 0;
    const raw = (label.getAttribute('aria-label') || label.innerText || '').trim().toLowerCase();
    const m = raw.replace(/,/g, '').match(/([\d.]+)\s*([kmb])?/);
    if (!m) return 0;
    const n = Number.parseFloat(m[1]);
    if (Number.isNaN(n)) return 0;
    const scale = { k: 1e3, m: 1e6, b: 1e9 }[m[2]] || 1;
    return Math.round(n * scale);
  };

  const articles = Array.from(document.querySelectorAll('article[data-testid="tweet"]'));

  return articles.slice(0, limit).map(article => {
    const time = article.querySelector('time');
    const link = time?.closest('a[href*="/status/"]') || article.querySelector('a[href*="/status/"]');
    const nameBlock = article.querySelector('[data-testid="User-Name"]');
    const spans = nameBlock
      ? Array.from(nameBlock.querySelectorAll('span')).map(s => (s.textContent || '').trim()).filter(Boolean)
      : [];
    const handle = spans.find(s => s.startsWith('@')) || '';

    // Many posts carry only images. Avatars and emoji live outside
    // tweetPhoto/card wrappers, so scoping to those keeps them out.
    const media = Array.from(
      article.querySelectorAll('[data-testid="tweetPhoto"] img, [data-testid="card.wrapper"] img')
    )
      .map(img => ({ url: img.getAttribute('src') || '', alt: img.getAttribute('alt') || '' }))
      .filter(m => m.url && !m.url.includes('profile_images') && !m.url.includes('/emoji/'));

    // X Articles are long-form posts. They render no tweetText at all -- the
    // headline and body live under their own testids -- so reading only
    // tweetText returned an article as empty.
    const articleTitle = article.querySelector('[data-testid="twitter-article-title"]')?.innerText || '';
    const longform = article.querySelector(
      '[data-testid="longformRichTextComponent"], [data-testid="twitterArticleRichTextView"]'
    )?.innerText || '';

    const tweetText = article.querySelector('[data-testid="tweetText"]')?.innerText || '';

    return {
      href: link ? (link.getAttribute('href') || '') : '',
      title: articleTitle,
      // An article body can run to many thousands of words; cap it so one post
      // cannot dominate a response.
      text: (tweetText || longform).slice(0, 6000),
      created_at: time ? (time.getAttribute('datetime') || '') : '',
      handle,
      name: spans.find(s => !s.startsWith('@')) || handle.replace(/^@/, ''),
      replies: count(article.querySelector('[data-testid="reply"]')),
      reposts: count(article.querySelector('[data-testid="retweet"], [data-testid="unretweet"]')),
      likes: count(article.querySelector('[data-testid="like"], [data-testid="unlike"]')),
      media
    };
  });
}`

// ScrollScript advances the timeline by roughly one viewport, which is how X
// loads more posts.
const ScrollScript = `() => {
  const step = Math.max(window.innerHeight * 0.9, 700);
  window.scrollBy(0, step);
  return document.documentElement.scrollTop || document.body.scrollTop || 0;
}`
