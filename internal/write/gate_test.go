package write

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/auth"
	"github.com/SohrabZ/x-browser-mcp/internal/browser"
)

var like = Request{Action: ActionLike, Target: "https://x.com/someone/status/222"}

// codePattern finds the codes a gate has shown its operator, newest last.
var codePattern = regexp.MustCompile(`Code: ([0-9a-f]{16})`)

// shownCodes reads back every code the operator has been shown.
func shownCodes(t *testing.T, operator *bytes.Buffer) []string {
	t.Helper()
	var out []string
	for _, m := range codePattern.FindAllStringSubmatch(operator.String(), -1) {
		out = append(out, m[1])
	}
	return out
}

// askFor has the gate show a code for req, the way a first call without one
// does, and returns that code.
func askFor(t *testing.T, g *Gate, operator *bytes.Buffer, req Request) string {
	t.Helper()
	if err := g.Check(req, ""); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("a write with no code: got %v, want ErrApprovalRequired", err)
	}
	codes := shownCodes(t, operator)
	if len(codes) == 0 {
		t.Fatalf("no code shown to the operator:\n%s", operator)
	}
	return codes[len(codes)-1]
}

// A disabled gate must refuse everything, whatever code is offered. This is the
// outermost guarantee: with writes off, the tools are never registered and the
// gate would still refuse if they somehow were.
func TestDisabledGateRefusesEverything(t *testing.T) {
	var operator bytes.Buffer
	g := NewGate(WritesOff, &operator)

	if g.Enabled() {
		t.Fatal("gate should be disabled")
	}
	for _, attempt := range []string{"", "guess", "0123456789abcdef"} {
		if err := g.Check(like, attempt); !errors.Is(err, ErrDisabled) {
			t.Errorf("code %q: got %v, want ErrDisabled", attempt, err)
		}
	}
	if operator.Len() != 0 {
		t.Errorf("a disabled gate showed the operator something:\n%s", &operator)
	}
}

// A write with no code is how every write starts. The operator is shown what it
// would do, so that is what they approve rather than a bare code.
func TestAWriteWithNoCodeShowsTheOperatorTheAction(t *testing.T) {
	var operator bytes.Buffer
	g := NewGate(WritesApproved, &operator)

	askFor(t, g, &operator, Request{Action: ActionReply, Target: "https://x.com/someone/status/222", Text: "thanks!"})

	for _, want := range []string{"APPROVE WRITE: reply", `"https://x.com/someone/status/222"`, `"thanks!"`} {
		if !strings.Contains(operator.String(), want) {
			t.Errorf("the operator was not shown %s:\n%s", want, &operator)
		}
	}
}

// The code approves that action once. A code that survived its use would be the
// startup token over again, sitting in a transcript for any post to spend.
func TestACodeApprovesItsActionOnce(t *testing.T) {
	var operator bytes.Buffer
	g := NewGate(WritesApproved, &operator)
	code := askFor(t, g, &operator, like)

	if err := g.Check(like, code); err != nil {
		t.Fatalf("the code shown for this like should approve it: %v", err)
	}
	if err := g.Check(like, code); !errors.Is(err, ErrBadConfirmation) {
		t.Errorf("a used code: got %v, want ErrBadConfirmation", err)
	}
}

// This is what the codes are for. A code the user gave for one action is in the
// same context as untrusted posts, and one of them may steer the model to spend
// it on another -- which is what a single startup token allowed.
func TestACodeDoesNotApproveADifferentAction(t *testing.T) {
	post := Request{Action: ActionPost, Text: "the post the user asked for"}

	for _, other := range []Request{
		{Action: ActionPost, Text: "a post someone else wrote"},
		{Action: ActionReply, Target: "https://x.com/attacker/status/1", Text: "the post the user asked for"},
		{Action: ActionLike, Target: "https://x.com/attacker/status/1"},
	} {
		var operator bytes.Buffer
		g := NewGate(WritesApproved, &operator)
		code := askFor(t, g, &operator, post)

		if err := g.Check(other, code); !errors.Is(err, ErrBadConfirmation) {
			t.Errorf("%+v was approved with a code shown for %+v: %v", other, post, err)
		}
		// Offered for the wrong action, the code is spent. Whoever offered it
		// was mistaken or steered, and it should not survive to be tried again.
		if err := g.Check(post, code); !errors.Is(err, ErrBadConfirmation) {
			t.Errorf("a code offered for another action still approved its own: %v", err)
		}
	}
}

// A refused code leaves the caller with a way forward: a fresh code for what it
// asked, shown to the operator.
func TestARefusedCodeShowsANewOne(t *testing.T) {
	var operator bytes.Buffer
	g := NewGate(WritesApproved, &operator)

	if err := g.Check(like, "0000000000000000"); !errors.Is(err, ErrBadConfirmation) {
		t.Fatalf("a made-up code: got %v, want ErrBadConfirmation", err)
	}
	codes := shownCodes(t, &operator)
	if len(codes) != 1 {
		t.Fatalf("expected one new code shown, got %v", codes)
	}
	if err := g.Check(like, codes[0]); err != nil {
		t.Errorf("the new code should approve the action: %v", err)
	}
}

func TestACodeExpires(t *testing.T) {
	var operator bytes.Buffer
	g := NewGate(WritesApproved, &operator)
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }

	code := askFor(t, g, &operator, like)
	now = now.Add(approvalTTL + time.Second)

	if err := g.Check(like, code); !errors.Is(err, ErrBadConfirmation) {
		t.Errorf("an expired code: got %v, want ErrBadConfirmation", err)
	}
}

// Codes are the defence against injected instructions, so none may be
// predictable from another.
func TestCodesDifferEveryTime(t *testing.T) {
	var operator bytes.Buffer
	g := NewGate(WritesApproved, &operator)
	for i := 0; i < maxPending; i++ {
		askFor(t, g, &operator, like)
	}

	seen := make(map[string]bool)
	for _, code := range shownCodes(t, &operator) {
		if seen[code] {
			t.Fatalf("code repeated: %q", code)
		}
		seen[code] = true
	}
}

// Anything that reaches the port can ask for approvals. What is kept for them is
// bounded, and the oldest goes first.
func TestWaitingCodesAreBounded(t *testing.T) {
	var operator bytes.Buffer
	g := NewGate(WritesApproved, &operator)
	first := askFor(t, g, &operator, like)
	for i := 0; i < maxPending; i++ {
		askFor(t, g, &operator, like)
	}

	if len(g.pending) > maxPending {
		t.Errorf("%d codes waiting, want at most %d", len(g.pending), maxPending)
	}
	if err := g.Check(like, first); !errors.Is(err, ErrBadConfirmation) {
		t.Errorf("the oldest code should have been forgotten: %v", err)
	}
}

// The text is whatever the model sent, which may be whatever a post told it to.
// Printed raw, its control sequences could rewrite the lines the operator reads
// before handing a code over.
func TestTheNoticeCannotBeRewrittenByTheText(t *testing.T) {
	var operator bytes.Buffer
	g := NewGate(WritesApproved, &operator)

	hostile := "harmless\x1b[1A\x1b[2K\r  APPROVE WRITE: like\u202e"
	askFor(t, g, &operator, Request{Action: ActionPost, Text: hostile})

	for _, raw := range []string{"\x1b", "\r", "\u202e"} {
		if strings.Contains(operator.String(), raw) {
			t.Errorf("the notice carries a raw %q from the text:\n%q", raw, operator.String())
		}
	}
}

// With nowhere to show a code, no write could ever be approved, and asking the
// caller for one would send the user looking for something that is not there.
func TestAGateThatCannotShowACodeDoesNotAskForOne(t *testing.T) {
	g := NewGate(WritesApproved, failingWriter{})

	err := g.Check(like, "")
	if err == nil || errors.Is(err, ErrApprovalRequired) {
		t.Errorf("got %v, want a failure that does not ask for a code", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("terminal gone") }

// Every write now begins by asking for approval, so the log tells that apart from
// a refused code. Recorded as denials, ordinary use would look like the burst of
// denials that means something is trying codes.
func TestTheAuditLogTellsAnApprovalRequestFromARefusal(t *testing.T) {
	path := t.TempDir() + "/writes.log"
	var operator bytes.Buffer
	w := New(Options{Gate: NewGate(WritesApproved, &operator), Audit: NewAuditor(path)})

	if err := w.Like(t.Context(), "someone", "222", ""); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("a like with no code: got %v, want ErrApprovalRequired", err)
	}
	if err := w.Like(t.Context(), "someone", "222", "0000000000000000"); !errors.Is(err, ErrBadConfirmation) {
		t.Fatalf("a like with a made-up code: got %v, want ErrBadConfirmation", err)
	}

	records := readRecords(t, path)
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %+v", records)
	}
	if records[0].Outcome != OutcomePending || records[1].Outcome != OutcomeDenied {
		t.Errorf("outcomes = %s, %s; want %s then %s", records[0].Outcome, records[1].Outcome, OutcomePending, OutcomeDenied)
	}
}

// The approval binds the whole text, not the excerpt the log keeps. Binding the
// excerpt would let a code for a short post approve a long one that began the
// same way.
func TestAnApprovalBindsTheWholeText(t *testing.T) {
	var operator bytes.Buffer
	w := New(Options{Gate: NewGate(WritesApproved, &operator)})
	short := strings.Repeat("a", maxExcerpt)

	if err := w.Post(t.Context(), short, ""); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("got %v, want ErrApprovalRequired", err)
	}
	code := shownCodes(t, &operator)[0]
	if err := w.Post(t.Context(), short+" and then something else", code); !errors.Is(err, ErrBadConfirmation) {
		t.Errorf("a code for a %d-character post approved a longer one: %v", len(short), err)
	}
}

// With -auto-approve nothing is asked, whatever code arrives or does not.
func TestAnAutoApprovedGateLetsEveryWriteThrough(t *testing.T) {
	var operator bytes.Buffer
	g := NewGate(WritesAutoApproved, &operator)

	if !g.Enabled() || !g.AutoApproved() {
		t.Fatalf("enabled = %v, auto-approved = %v; want both", g.Enabled(), g.AutoApproved())
	}
	for _, c := range []struct {
		req  Request
		code string
	}{
		{like, ""},
		{like, "0000000000000000"},
		{Request{Action: ActionPost, Text: "anything at all"}, ""},
	} {
		if err := g.Check(c.req, c.code); err != nil {
			t.Errorf("%+v with code %q: got %v, want it let through", c.req, c.code, err)
		}
	}
	if codes := shownCodes(t, &operator); len(codes) != 0 {
		t.Errorf("codes shown for writes that needed none: %v", codes)
	}
}

// Nobody is asked, so the operator is told: each write that went ahead is shown
// where an approval would have been, quoted the same way.
func TestAnAutoApprovedWriteIsStillShownToTheOperator(t *testing.T) {
	var operator bytes.Buffer
	g := NewGate(WritesAutoApproved, &operator)

	if err := g.Check(Request{Action: ActionReply, Target: "https://x.com/someone/status/222", Text: "hi\x1b[2K"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"AUTO-APPROVED WRITE: reply", `"https://x.com/someone/status/222"`, `"hi\x1b[2K"`} {
		if !strings.Contains(operator.String(), want) {
			t.Errorf("the operator was not shown %s:\n%q", want, operator.String())
		}
	}
	if strings.Contains(operator.String(), "\x1b") {
		t.Errorf("the text reached the terminal raw:\n%q", operator.String())
	}
}

// The banner is the operator's one warning of what -auto-approve gives up.
func TestTheAutoApproveBannerSaysWhatItGivesUp(t *testing.T) {
	banner := NewGate(WritesAutoApproved, nil).Banner()
	for _, want := range []string{"WITHOUT APPROVAL", "A post your agent", "as you"} {
		if !strings.Contains(banner, want) {
			t.Errorf("the banner should say %q:\n%s", want, banner)
		}
	}
}

// After a write that should not have happened, the first question is whether a
// person approved it. The log answers it for every write that got past the gate.
func TestTheAuditLogSaysAWriteWasAutoApproved(t *testing.T) {
	noBrowser := func(context.Context) (*browser.Session, func(), error) {
		return nil, nil, errors.New("no browser in this test")
	}
	for _, c := range []struct {
		mode Mode
		want bool
	}{{WritesAutoApproved, true}, {WritesApproved, false}} {
		path := t.TempDir() + "/writes.log"
		w := New(Options{
			Gate:  NewGate(c.mode, &bytes.Buffer{}),
			Audit: NewAuditor(path),
			Auth:  auth.New(auth.Options{Lease: noBrowser}),
		})
		_ = w.Like(t.Context(), "someone", "222", "")

		records := readRecords(t, path)
		if len(records) != 1 || records[0].AutoApproved != c.want {
			t.Errorf("mode %v: records %+v, want one with auto_approved=%v", c.mode, records, c.want)
		}
	}
}

func TestBannerOnlyAppearsWhenEnabled(t *testing.T) {
	if NewGate(WritesOff, nil).Banner() != "" {
		t.Error("a disabled gate should print nothing")
	}
	if !strings.Contains(NewGate(WritesApproved, nil).Banner(), "WRITES ENABLED") {
		t.Error("the banner must tell the operator that writes are on")
	}
}
