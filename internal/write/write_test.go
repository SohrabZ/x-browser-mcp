package write

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/browser"
	"github.com/SohrabZ/x-browser-mcp/internal/xui"
)

// A disabled gate must refuse everything, whatever token is offered. This is
// the outermost guarantee: with writes off, the tools are never registered and
// the gate would still refuse if they somehow were.
func TestDisabledGateRefusesEverything(t *testing.T) {
	g, err := NewGate(false)
	if err != nil {
		t.Fatalf("new gate: %v", err)
	}

	if g.Enabled() {
		t.Fatal("gate should be disabled")
	}
	for _, attempt := range []string{"", "guess", g.Token()} {
		if err := g.Check(attempt); !errors.Is(err, ErrDisabled) {
			t.Errorf("token %q: got %v, want ErrDisabled", attempt, err)
		}
	}
}

func TestEnabledGateAcceptsOnlyItsToken(t *testing.T) {
	g, err := NewGate(true)
	if err != nil {
		t.Fatalf("new gate: %v", err)
	}

	if err := g.Check(g.Token()); err != nil {
		t.Fatalf("the real token should pass: %v", err)
	}
	for _, wrong := range []string{"", "0000000000000000", strings.ToUpper(g.Token())} {
		if err := g.Check(wrong); !errors.Is(err, ErrBadConfirmation) {
			t.Errorf("token %q: got %v, want ErrBadConfirmation", wrong, err)
		}
	}
}

// The token is the defence against injected instructions, so it must not be
// predictable across runs.
func TestTokensDifferBetweenGates(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		g, err := NewGate(true)
		if err != nil {
			t.Fatalf("new gate: %v", err)
		}
		if g.Token() == "" {
			t.Fatal("an enabled gate must have a token")
		}
		if seen[g.Token()] {
			t.Fatalf("token repeated across gates: %q", g.Token())
		}
		seen[g.Token()] = true
	}
}

// An empty token must never authorise, or a failed mint would open the gate.
func TestBlankTokenNeverAuthorises(t *testing.T) {
	g := &Gate{enabled: true} // token deliberately unset

	if err := g.Check(""); !errors.Is(err, ErrDisabled) {
		t.Fatalf("a gate with no token must refuse, got %v", err)
	}
}

func TestBannerOnlyAppearsWhenEnabled(t *testing.T) {
	off, _ := NewGate(false)
	if off.Banner() != "" {
		t.Error("a disabled gate should print nothing")
	}

	on, _ := NewGate(true)
	if !strings.Contains(on.Banner(), on.Token()) {
		t.Error("the banner must show the operator the token")
	}
}

func TestValidateText(t *testing.T) {
	if err := ValidateText(""); err == nil {
		t.Error("empty text should be rejected")
	}
	if err := ValidateText("   \n "); err == nil {
		t.Error("whitespace-only text should be rejected")
	}
	if err := ValidateText("hello"); err != nil {
		t.Errorf("ordinary text should pass: %v", err)
	}
	if err := ValidateText(strings.Repeat("a", MaxPostRunes)); err != nil {
		t.Errorf("text at the limit should pass: %v", err)
	}
	if err := ValidateText(strings.Repeat("a", MaxPostRunes+1)); err == nil {
		t.Error("over-long text should be rejected")
	}
}

// The limit is characters, not bytes: a post of multi-byte characters well
// under the limit must not be rejected for its byte length.
func TestValidateTextCountsRunesNotBytes(t *testing.T) {
	text := strings.Repeat("چ", 200) // 200 runes, far more bytes

	if err := ValidateText(text); err != nil {
		t.Fatalf("200 characters should be accepted: %v", err)
	}
}

func readRecords(t *testing.T, path string) []Record {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	defer f.Close()

	var out []Record
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var rec Record
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatalf("decode record: %v", err)
		}
		out = append(out, rec)
	}
	return out
}

func TestAuditorAppendsRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writes.log")
	a := NewAuditor(path)

	if err := a.Log(Record{Action: ActionPost, Outcome: OutcomeOK, Excerpt: "hello"}); err != nil {
		t.Fatalf("log: %v", err)
	}
	if err := a.Log(Record{Action: ActionLike, Outcome: OutcomeFailed, Reason: "boom"}); err != nil {
		t.Fatalf("log: %v", err)
	}

	records := readRecords(t, path)
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}
	if records[0].Action != ActionPost || records[0].Outcome != OutcomeOK {
		t.Errorf("unexpected first record: %+v", records[0])
	}
	if records[1].Reason != "boom" {
		t.Errorf("failure reason should be kept: %+v", records[1])
	}
	if records[0].At.IsZero() {
		t.Error("records should be timestamped")
	}
}

// Denied attempts are the ones worth having: a burst of them is the signal that
// something is trying to drive the account.
func TestAuditorRecordsDenials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writes.log")
	a := NewAuditor(path)

	if err := a.Log(Record{Action: ActionPost, Outcome: OutcomeDenied, Reason: "bad token"}); err != nil {
		t.Fatalf("log: %v", err)
	}

	records := readRecords(t, path)
	if len(records) != 1 || records[0].Outcome != OutcomeDenied {
		t.Fatalf("expected a denial to be recorded, got %+v", records)
	}
}

func TestAuditorTruncatesExcerpts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "writes.log")
	a := NewAuditor(path)

	if err := a.Log(Record{Action: ActionPost, Excerpt: strings.Repeat("x", 500)}); err != nil {
		t.Fatalf("log: %v", err)
	}

	records := readRecords(t, path)
	if n := len([]rune(records[0].Excerpt)); n > maxExcerpt {
		t.Fatalf("excerpt not truncated: %d runes", n)
	}
}

// The log holds post content, so it must not be world-readable.
func TestAuditLogIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "writes.log")
	a := NewAuditor(path)

	if err := a.Log(Record{Action: ActionPost}); err != nil {
		t.Fatalf("log: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("got mode %o, want 600", perm)
	}
}

func TestNilAuditorIsSafe(t *testing.T) {
	var a *Auditor
	if err := a.Log(Record{Action: ActionPost}); err != nil {
		t.Fatalf("a nil auditor should be a no-op, got %v", err)
	}
}

// A write browser that would not confirm its exit keeps the profile. Handing it
// back would put the next read's Chrome on a directory the write browser may
// still own -- and the session cannot answer the question, because Close has
// already torn its connection down. The profile itself is asked instead.
func TestAnUnconfirmedWriteShutdownHoldsTheProfile(t *testing.T) {
	dir := t.TempDir()

	// A live process holding the lock stands in for the browser that would not go.
	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill() })
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	if err := os.Symlink(fmt.Sprintf("%s-%d", host, holder.Process.Pid), filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	res := &recordingReservation{}
	holdUntilFree(errors.New("chrome did not exit"), dir, res)

	time.Sleep(300 * time.Millisecond)
	if res.released.Load() {
		t.Fatal("the profile was handed back while a process still held it")
	}

	// Once the holder is gone, the profile is released without further prompting.
	_ = holder.Process.Kill()
	_ = holder.Wait()

	deadline := time.After(5 * time.Second)
	for !res.released.Load() {
		select {
		case <-deadline:
			t.Fatal("the profile was never released after its holder exited")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// A confirmed shutdown hands the profile straight back; the hold is for doubt,
// not for every write.
func TestAConfirmedWriteShutdownReleasesImmediately(t *testing.T) {
	res := &recordingReservation{}
	holdUntilFree(nil, t.TempDir(), res)
	if !res.released.Load() {
		t.Fatal("a confirmed shutdown should release the profile at once")
	}
}

type recordingReservation struct{ released atomic.Bool }

func (r *recordingReservation) Release() { r.released.Store(true) }

// A permalink is not one post. X renders the post's ancestors above it and its
// replies below, each with a reply, repost, like and bookmark row of its own, so
// a check matched against the document answers for whichever of them the page
// happens to reach first. One reply the viewer had already liked was enough to
// make a like report success without ever touching the post that was asked for.
func TestAnActionReadsOnlyThePostItWasAskedFor(t *testing.T) {
	p, done := postPage(t, `
		<article data-testid="tweet" id="ancestor">
			<a href="/someone/status/111">1h</a>
			<div data-testid="unlike" onclick="press('ancestor')">liked already</div>
			<div data-testid="removeBookmark">bookmarked already</div>
		</article>
		<article data-testid="tweet" id="wanted">
			<a href="/someone/status/222">2h</a>
			<div data-testid="like" onclick="press('wanted')">like</div>
			<div data-testid="bookmark">bookmark</div>
		</article>
		<article data-testid="tweet" id="reply">
			<a href="/other/status/333">3h</a>
			<div data-testid="unlike" onclick="press('reply')">liked already</div>
		</article>`)
	defer done()

	// The document holds an unlike and a removeBookmark, both on other posts.
	// Neither says anything about the post that was asked for.
	if hasOnPost(p, "222", xui.SelUnlikeButton, 200*time.Millisecond) {
		t.Error("read another post's like as this one's; a like would report success having done nothing")
	}
	if hasOnPost(p, "222", xui.SelBookmarkRemove, 200*time.Millisecond) {
		t.Error("read another post's bookmark as this one's")
	}

	// And the one it does hold is found.
	if !hasOnPost(p, "222", xui.SelLikeButton, time.Second) {
		t.Error("did not find the like control on the post that has one")
	}

	if err := pressOnPost(p, "222", xui.SelLikeButton, time.Second); err != nil {
		t.Fatalf("press: %v", err)
	}
	if got := pressed(t, p); got != "wanted" {
		t.Errorf("pressed %q, want the post that was asked for", got)
	}
}

// The control has to be on the post itself. Pressing whatever the document
// offers first is how the wrong post gets liked.
func TestPressingAControlThePostDoesNotOfferFails(t *testing.T) {
	p, done := postPage(t, `
		<article data-testid="tweet" id="wanted">
			<a href="/someone/status/222">2h</a>
			<div data-testid="unlike" onclick="press('wanted')">liked already</div>
		</article>
		<article data-testid="tweet" id="reply">
			<a href="/other/status/333">3h</a>
			<div data-testid="like" onclick="press('reply')">like</div>
		</article>`)
	defer done()

	err := pressOnPost(p, "222", xui.SelLikeButton, 300*time.Millisecond)
	if err == nil {
		t.Fatal("pressed a control the post does not have")
	}
	if got := pressed(t, p); got != "" {
		t.Errorf("pressed %q; nothing should have been pressed", got)
	}
}

// X leaves the status link off the post you are already looking at, so when no
// article claims the id the first one is used -- the post itself on a top-level
// permalink, and what an unscoped selector would have found anyway.
func TestAPostThatClaimsNoIDFallsBackToTheFirstOne(t *testing.T) {
	p, done := postPage(t, `
		<article data-testid="tweet" id="wanted">
			<div data-testid="like" onclick="press('wanted')">like</div>
		</article>
		<article data-testid="tweet" id="reply">
			<a href="/other/status/333">3h</a>
			<div data-testid="like" onclick="press('reply')">like</div>
		</article>`)
	defer done()

	if err := pressOnPost(p, "222", xui.SelLikeButton, time.Second); err != nil {
		t.Fatalf("press: %v", err)
	}
	if got := pressed(t, p); got != "wanted" {
		t.Errorf("pressed %q, want the first post on the page", got)
	}
}

// An empty page is reported as such rather than as a post without the control,
// so the wait can tell "X has not rendered yet" from "this post cannot do that".
func TestNoPostAtAllIsNotAMissingControl(t *testing.T) {
	p, done := postPage(t, `<div>no posts here</div>`)
	defer done()

	state, err := onPost(p, "222", xui.SelLikeButton, false)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if state != noPost {
		t.Errorf("state %q, want %q", state, noPost)
	}
}

// postPage serves the given articles and drives a real browser at them, which is
// the only way to exercise a selector: the scoping lives in the page.
func postPage(t *testing.T, body string) (*browser.Page, func()) {
	t.Helper()
	if testing.Short() {
		t.Skip("needs a browser")
	}
	chrome := browser.ChromePathForTest()
	if chrome == "" {
		t.Skip("no Chrome installed")
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprintf(w, `<html><body>
			<script>window.pressedID = ''; function press(id) { window.pressedID = id; }</script>
			%s
		</body></html>`, body)
	}))

	session, err := browser.Open(context.Background(), browser.Options{ChromePath: chrome, Headless: true})
	if err != nil {
		srv.Close()
		t.Skipf("cannot start chrome: %v", err)
	}

	page, err := session.Page(context.Background())
	if err != nil {
		_ = session.Close()
		srv.Close()
		t.Fatalf("page: %v", err)
	}
	if err := page.Goto(srv.URL); err != nil {
		page.Close()
		_ = session.Close()
		srv.Close()
		t.Fatalf("goto: %v", err)
	}

	return page, func() {
		page.Close()
		_ = session.Close()
		srv.Close()
	}
}

// pressed reports which post recorded a press, or "" if none did.
func pressed(t *testing.T, p *browser.Page) string {
	t.Helper()
	res, err := p.Rod().Eval(`() => window.pressedID`)
	if err != nil {
		t.Fatalf("read press: %v", err)
	}
	return res.Value.String()
}
