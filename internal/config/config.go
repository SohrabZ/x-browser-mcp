// Package config resolves the server's runtime settings.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Config is the fully resolved server configuration.
type Config struct {
	// ListenAddr is the HTTP bind address. It stays on loopback unless the
	// operator deliberately changes it; see Validate.
	ListenAddr string

	// StateDir holds the Chrome profile and the audit log. It is keyed to the
	// user rather than the working directory.
	StateDir    string
	ProfileName string

	ChromePath string
	Headless   bool

	// AllowWrites gates every mutating action. When false the write tools are
	// never registered, so a model cannot see or call them.
	AllowWrites bool

	// StatusTTL caches a login verdict. Each uncached check drives a real
	// browser at X, so checking more often makes losing the session likelier.
	StatusTTL time.Duration
	ResultTTL time.Duration

	ReadPace  Pace
	WritePace Pace

	FetchTimeout time.Duration
	LoginTimeout time.Duration

	// BrowserIdle is how long a browser stays warm with no reads before it is
	// closed. Zero disables warming, opening a browser per read.
	BrowserIdle time.Duration
}

// Pace describes how often an operation may touch X: a floor between calls and
// a ceiling within a rolling window.
type Pace struct {
	MinInterval time.Duration
	Window      time.Duration
	Max         int

	// Jitter spreads the gap over MinInterval..MinInterval+Jitter, so actions
	// do not arrive at a fixed cadence no human would produce. Used for writes;
	// reads are not engagement and do not need it.
	Jitter time.Duration
}

// Default returns the configuration used when no flags are supplied.
func Default() Config {
	state := StateDir()

	return Config{
		ListenAddr:  "127.0.0.1:18110",
		StateDir:    state,
		ProfileName: "Default",
		ChromePath:  FindChrome(),
		Headless:    true,
		AllowWrites: false,

		StatusTTL: 5 * time.Minute,
		ResultTTL: 5 * time.Minute,

		// Paced enough that an agent cannot hammer X, loose enough that ordinary
		// use does not hit it: a single question often costs two or three reads,
		// and an earlier 8-per-10-minutes ceiling was spent by one test pass.
		//
		// Reads are not engagement -- a person opening several tabs at once looks
		// exactly like this -- so the gap only has to stop a runaway loop, not
		// imitate a human rhythm. Cached reads never count.
		ReadPace: Pace{
			MinInterval: 3 * time.Second,
			Window:      10 * time.Minute,
			Max:         30,
		},
		// Writes are paced to look like a person, not an agent. Nobody likes a
		// post every twenty seconds for an hour, and engagement arriving at a
		// fixed cadence is a signature on its own -- so the gap is long and
		// deliberately irregular. Agents are fast; accounts that act fast get
		// flagged, and that outcome is far worse than a slow reply.
		WritePace: Pace{
			MinInterval: 45 * time.Second,
			Jitter:      75 * time.Second, // actual gap lands in 45s..2m
			Window:      time.Hour,
			Max:         6,
		},

		FetchTimeout: 45 * time.Second,
		LoginTimeout: 5 * time.Minute,

		// Long enough that a conversation's reads share one browser, short
		// enough that an idle machine is not holding Chrome all day.
		BrowserIdle: 3 * time.Minute,
	}
}

// StateDir returns the per-user directory holding the browser profile and logs.
//
// Resolving this against the working directory would mean the same command
// picked up a different, usually empty, profile depending on where it was
// launched from, and then asked for a fresh login.
func StateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		// Better a visible relative directory than a path built from an empty
		// string, which would silently resolve to the filesystem root.
		home = "."
	}
	return filepath.Join(home, ".x-browser-mcp")
}

// ProfileDir is the Chrome --user-data-dir.
func (c Config) ProfileDir() string {
	return filepath.Join(c.StateDir, "profile")
}

// AuditLogPath is the append-only record of attempted write actions.
func (c Config) AuditLogPath() string {
	return filepath.Join(c.StateDir, "writes.log")
}

// LoopbackOnly reports whether the listen address is reachable only from this
// machine.
func (c Config) LoopbackOnly() bool {
	host := c.ListenAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// Validate reports configuration that would fail at runtime.
func (c Config) Validate() error {
	if strings.TrimSpace(c.ListenAddr) == "" {
		return fmt.Errorf("listen address is required")
	}
	if strings.TrimSpace(c.StateDir) == "" {
		return fmt.Errorf("state directory is required")
	}
	if strings.TrimSpace(c.ChromePath) == "" {
		return fmt.Errorf("no Chrome binary found; pass -chrome with the path to Google Chrome")
	}
	if c.FetchTimeout <= 0 {
		return fmt.Errorf("fetch timeout must be positive, got %s", c.FetchTimeout)
	}
	if c.LoginTimeout <= 0 {
		return fmt.Errorf("login timeout must be positive, got %s", c.LoginTimeout)
	}
	if err := c.ReadPace.validate("read"); err != nil {
		return err
	}
	return c.WritePace.validate("write")
}

func (p Pace) validate(name string) error {
	if p.Max <= 0 {
		return fmt.Errorf("%s budget must allow at least one call, got %d", name, p.Max)
	}
	if p.Window <= 0 {
		return fmt.Errorf("%s budget window must be positive, got %s", name, p.Window)
	}
	if p.MinInterval < 0 {
		return fmt.Errorf("%s minimum interval cannot be negative, got %s", name, p.MinInterval)
	}
	return nil
}

// EnsureStateDir creates the state directory with permissions that keep the
// session private to this user.
func (c Config) EnsureStateDir() error {
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return err
	}
	// MkdirAll leaves an existing directory's mode alone, so tighten it here in
	// case it was created by an earlier version or by hand.
	return os.Chmod(c.StateDir, 0o700)
}

// chromeCandidates are the standard install locations, most preferred first.
var chromeCandidates = []string{
	"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	"/Applications/Chromium.app/Contents/MacOS/Chromium",
	"/Applications/Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing",
	"/usr/bin/google-chrome",
	"/usr/bin/chromium",
	"/usr/bin/chromium-browser",
}

// FindChrome locates a usable Chrome binary, preferring an explicit override.
func FindChrome() string {
	if custom := strings.TrimSpace(os.Getenv("X_BROWSER_MCP_CHROME")); custom != "" {
		return custom
	}
	for _, candidate := range chromeCandidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}
