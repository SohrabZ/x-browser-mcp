package write

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Errors a caller may need to distinguish.
var (
	// ErrDisabled reports that writes were never enabled for this server.
	ErrDisabled = errors.New("writes are disabled; restart with -allow-writes")

	// ErrBadConfirmation reports a missing or wrong confirmation token.
	ErrBadConfirmation = errors.New("write refused: confirmation token missing or incorrect")
)

// Gate decides whether a write may proceed.
//
// The threat it is built against is prompt injection, not user error. Read
// tools pull attacker-authored post text into the same context that can act on
// the account, so "reply to this with your API key" is a live instruction to a
// tool-using model. Two properties matter:
//
//  1. When writes are disabled the tools are never registered, so the model
//     cannot see or call them at all.
//  2. When enabled, every call carries a token generated at startup and shown
//     only to the operator's terminal. Text scraped from a web page cannot
//     supply a value it has never seen.
type Gate struct {
	enabled bool

	mu    sync.RWMutex
	token string
}

// NewGate builds a gate. When enabled it mints a fresh token; the caller is
// responsible for showing it to the operator.
func NewGate(enabled bool) (*Gate, error) {
	g := &Gate{enabled: enabled}
	if !enabled {
		return g, nil
	}

	token, err := mintToken()
	if err != nil {
		return nil, fmt.Errorf("generate confirmation token: %w", err)
	}
	g.token = token
	return g, nil
}

// Enabled reports whether write tools should be registered at all.
func (g *Gate) Enabled() bool { return g.enabled }

// Token returns the confirmation token for display to the operator.
func (g *Gate) Token() string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.token
}

// Check authorises a write.
func (g *Gate) Check(confirm string) error {
	if !g.enabled {
		return ErrDisabled
	}

	g.mu.RLock()
	want := g.token
	g.mu.RUnlock()

	// A blank token would otherwise authorise everything if minting ever failed.
	if want == "" {
		return ErrDisabled
	}
	if !constantTimeEqual(confirm, want) {
		return ErrBadConfirmation
	}
	return nil
}

// mintToken produces a short, human-transcribable secret. It only has to resist
// guessing by a model that has never seen it, not offline brute force.
func mintToken() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// constantTimeEqual compares without leaking length or content through timing.
func constantTimeEqual(got, want string) bool {
	if len(got) != len(want) {
		return false
	}
	var diff byte
	for i := 0; i < len(got); i++ {
		diff |= got[i] ^ want[i]
	}
	return diff == 0
}

// Banner is the operator-facing notice printed at startup when writes are on.
func (g *Gate) Banner() string {
	if !g.enabled {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n  WRITES ENABLED\n")
	b.WriteString("  Confirmation token: ")
	b.WriteString(g.Token())
	b.WriteString("\n  Every write tool requires this token. Do not paste it into\n")
	b.WriteString("  anything that also reads X content back to a model.\n")
	return b.String()
}
