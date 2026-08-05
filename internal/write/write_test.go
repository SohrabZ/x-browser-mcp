package write

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
