package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// State must be keyed to the user, not the working directory. A cwd-relative
// default means the same command silently picks up a different, usually empty,
// profile depending on where it was launched from, and then asks for a fresh
// login instead of reusing the existing session.
func TestStateDirIsIndependentOfWorkingDirectory(t *testing.T) {
	before := Default()

	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	after := Default()

	if before.StateDir != after.StateDir {
		t.Fatalf("state dir moved with the working directory: %q then %q", before.StateDir, after.StateDir)
	}
	if before.ProfileDir() != after.ProfileDir() {
		t.Fatalf("profile dir moved with the working directory: %q then %q", before.ProfileDir(), after.ProfileDir())
	}
	if home, err := os.UserHomeDir(); err == nil && !strings.HasPrefix(after.StateDir, home) {
		t.Fatalf("expected state under the home directory, got %q", after.StateDir)
	}
}

// Writes must never be on unless the operator asked, because the read tools
// pull attacker-authored text into the same context that can act on the account.
func TestWritesAreDisabledByDefault(t *testing.T) {
	if Default().AllowWrites {
		t.Fatal("writes must be opt-in")
	}
}

func TestDefaultBindsToLoopback(t *testing.T) {
	cfg := Default()
	if !cfg.LoopbackOnly() {
		t.Fatalf("default listen address must be loopback, got %q", cfg.ListenAddr)
	}
}

func TestLoopbackOnlyRecognisesExposedAddresses(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:18110": true,
		"localhost:18110": true,
		"[::1]:18110":     true,
		":18110":          false, // all interfaces
		"0.0.0.0:18110":   false,
		"192.168.1.5:80":  false,
	}
	for addr, want := range cases {
		cfg := Config{ListenAddr: addr}
		if got := cfg.LoopbackOnly(); got != want {
			t.Errorf("%s: got loopback=%v, want %v", addr, got, want)
		}
	}
}

// Writes get a tighter budget than reads on purpose; a config that inverted
// that would be a mistake worth catching.
func TestWriteBudgetIsStricterThanRead(t *testing.T) {
	cfg := Default()
	if cfg.WritePace.Max >= cfg.ReadPace.Max {
		t.Errorf("write budget (%d) should be smaller than read budget (%d)", cfg.WritePace.Max, cfg.ReadPace.Max)
	}
	if cfg.WritePace.MinInterval < cfg.ReadPace.MinInterval {
		t.Errorf("writes should be paced at least as slowly as reads")
	}
}

// The read budget has to survive ordinary use. A single question often costs
// two or three live reads, and an earlier ceiling of 8 per 10 minutes was spent
// by one pass over the read tools -- a limiter that normal use trips is a bug,
// not a safeguard.
func TestReadBudgetSurvivesARealisticSession(t *testing.T) {
	cfg := Default()

	if cfg.ReadPace.Max < 20 {
		t.Errorf("read budget of %d per %s is too tight for ordinary use", cfg.ReadPace.Max, cfg.ReadPace.Window)
	}
	// The floor between reads still has to allow a handful in quick succession
	// when an agent chains a few tool calls to answer one question.
	if cfg.ReadPace.MinInterval > 10*time.Second {
		t.Errorf("minimum interval of %s stalls chained tool calls", cfg.ReadPace.MinInterval)
	}
}

func TestValidateAcceptsDefaults(t *testing.T) {
	cfg := Default()
	if cfg.ChromePath == "" {
		cfg.ChromePath = "/nonexistent/chrome" // CI has no Chrome installed
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
}

func TestValidateRejectsBadConfig(t *testing.T) {
	base := func() Config {
		cfg := Default()
		cfg.ChromePath = "/nonexistent/chrome"
		return cfg
	}

	cases := map[string]func(*Config){
		"no listen addr":     func(c *Config) { c.ListenAddr = "" },
		"no state dir":       func(c *Config) { c.StateDir = "" },
		"no chrome":          func(c *Config) { c.ChromePath = "  " },
		"zero fetch":         func(c *Config) { c.FetchTimeout = 0 },
		"zero login":         func(c *Config) { c.LoginTimeout = 0 },
		"zero write":         func(c *Config) { c.WriteTimeout = 0 },
		"empty read budget":  func(c *Config) { c.ReadPace.Max = 0 },
		"empty write window": func(c *Config) { c.WritePace.Window = 0 },
	}

	for name, mutate := range cases {
		cfg := base()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("%s: expected validation to fail", name)
		}
	}
}

func TestEnsureStateDirIsPrivate(t *testing.T) {
	cfg := Default()
	cfg.StateDir = filepath.Join(t.TempDir(), "state")

	if err := cfg.EnsureStateDir(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	info, err := os.Stat(cfg.StateDir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("state dir holds the session; got mode %o, want 700", perm)
	}
}

// A pre-existing loose directory must be tightened, not left as found.
func TestEnsureStateDirTightensExistingDirectory(t *testing.T) {
	cfg := Default()
	cfg.StateDir = filepath.Join(t.TempDir(), "loose")
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := cfg.EnsureStateDir(); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	info, _ := os.Stat(cfg.StateDir)
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Fatalf("got mode %o, want 700", perm)
	}
}

func TestFindChromePrefersEnvironmentOverride(t *testing.T) {
	t.Setenv("X_BROWSER_MCP_CHROME", "/custom/chrome")
	if got := FindChrome(); got != "/custom/chrome" {
		t.Fatalf("got %q, want the override", got)
	}
}

func TestPathsHangOffStateDir(t *testing.T) {
	cfg := Default()
	cfg.StateDir = "/tmp/example"

	if got, want := cfg.ProfileDir(), "/tmp/example/profile"; got != want {
		t.Errorf("profile dir: got %q, want %q", got, want)
	}
	if got, want := cfg.AuditLogPath(), "/tmp/example/writes.log"; got != want {
		t.Errorf("audit log: got %q, want %q", got, want)
	}
}

func TestDefaultTimeoutsAreSane(t *testing.T) {
	cfg := Default()
	// The first read after an idle period cold-starts Chrome, which routinely
	// takes 10-20s, so a tight fetch timeout turns normal use into failures.
	if cfg.FetchTimeout < 30*time.Second {
		t.Errorf("fetch timeout %s is too tight for a cold browser start", cfg.FetchTimeout)
	}
	if cfg.LoginTimeout < time.Minute {
		t.Errorf("login timeout %s leaves no time to type credentials", cfg.LoginTimeout)
	}
	// A write starts its own browser, presses a control, waits for X's request,
	// and reloads the post to confirm the action survived. Each of those waits
	// has a bound of its own, and the deadline has to outlast their sum or it
	// truncates the confirmation and reports a timeout instead of a verdict.
	if cfg.WriteTimeout <= cfg.FetchTimeout {
		t.Errorf("write timeout %s does not allow for more than a read (%s), but a write verifies itself",
			cfg.WriteTimeout, cfg.FetchTimeout)
	}
}

// Writes must look like a person using the account, not an agent driving it.
// Bursts of engagement at a fixed cadence are what get accounts flagged, and
// that outcome is far worse than a slow reply.
func TestWritePaceIsHumanShaped(t *testing.T) {
	cfg := Default()

	if cfg.WritePace.MinInterval < 30*time.Second {
		t.Errorf("writes every %s is faster than a person acts", cfg.WritePace.MinInterval)
	}
	if cfg.WritePace.Jitter <= 0 {
		t.Error("a fixed gap between writes is a signature on its own; jitter must be set")
	}
	if cfg.WritePace.Max > 12 {
		t.Errorf("%d writes per %s is more engagement than a person produces", cfg.WritePace.Max, cfg.WritePace.Window)
	}
	// Reads are not engagement and are not paced this way.
	if cfg.ReadPace.Jitter != 0 {
		t.Error("reads do not need jitter")
	}
}
