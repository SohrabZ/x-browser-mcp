package write

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/model"
)

// Outcome is how a write attempt ended.
type Outcome string

const (
	OutcomeOK     Outcome = "ok"
	OutcomeDenied Outcome = "denied"
	OutcomeFailed Outcome = "failed"
)

// Record is one line of the audit log.
type Record struct {
	At      time.Time `json:"at"`
	Action  string    `json:"action"`
	Target  string    `json:"target,omitempty"`
	Excerpt string    `json:"excerpt,omitempty"`
	Outcome Outcome   `json:"outcome"`
	Reason  string    `json:"reason,omitempty"`
}

// Auditor appends write attempts to a file.
//
// Every attempt is recorded, including denied ones: a burst of denials is the
// signal that something is trying to drive the account, and it is exactly the
// evidence that would be missing if only successes were logged.
type Auditor struct {
	path string
	mu   sync.Mutex
}

// NewAuditor writes to path, creating its directory if needed.
func NewAuditor(path string) *Auditor {
	return &Auditor{path: path}
}

// maxExcerpt caps how much post text is copied into the log. Enough to identify
// what was written, not enough to make the log a second copy of the content.
const maxExcerpt = 120

// Log appends a record. Failures to log are returned but should not block the
// caller's decision — an unwritable log must not become an outage.
func (a *Auditor) Log(rec Record) error {
	if a == nil || a.path == "" {
		return nil
	}
	if rec.At.IsZero() {
		rec.At = time.Now().UTC()
	}
	rec.Excerpt = model.Excerpt(model.Normalize(rec.Excerpt), maxExcerpt)

	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write(append(line, '\n'))
	return err
}
