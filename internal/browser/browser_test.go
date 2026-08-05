package browser

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/launcher/flags"
)

func flagsOf(t *testing.T, opts Options) string {
	t.Helper()
	return strings.Join(Launcher(opts).FormatArgs(), " ")
}

// rod enables --use-mock-keychain by default. With it, Chrome cannot decrypt
// the cookies a real Chrome wrote: the logged-in profile reads back empty and
// is then re-encrypted with the mock key, destroying the saved session. A
// dependency bump could reintroduce it, and the only symptom would be "login
// does not stick", so it is pinned here.
func TestLauncherDisablesMockKeychain(t *testing.T) {
	args := flagsOf(t, Options{ProfileDir: t.TempDir(), ProfileName: "Default", Headless: true})

	if strings.Contains(args, mockKeychainFlag) {
		t.Fatalf("--%s must stay deleted: Chrome cannot decrypt the persisted session with a mock keychain, and destroys it on exit\ngot: %s", mockKeychainFlag, args)
	}
}

func TestLauncherUsesConfiguredProfile(t *testing.T) {
	dir := t.TempDir()
	args := flagsOf(t, Options{ProfileDir: dir, ProfileName: "Default", Headless: true})

	if !strings.Contains(args, "--user-data-dir="+dir) {
		t.Errorf("launcher must reuse the persistent profile\ngot: %s", args)
	}
	if !strings.Contains(args, "--"+profileDirFlag+"=Default") {
		t.Errorf("launcher must select the configured profile directory\ngot: %s", args)
	}
}

// With no profile of ours, rod still points Chrome at a throwaway directory of
// its own; the invariant is that we do not select a profile inside it.
func TestLauncherOmitsProfileFlagsWithoutProfileDir(t *testing.T) {
	args := flagsOf(t, Options{Headless: true})

	if strings.Contains(args, "--"+profileDirFlag+"=") {
		t.Errorf("must not select a profile directory when none is configured\ngot: %s", args)
	}
}

func TestLauncherHonoursChromePath(t *testing.T) {
	l := Launcher(Options{ChromePath: "/custom/chrome", Headless: true})
	if got := l.Get(flags.Bin); got != "/custom/chrome" {
		t.Fatalf("got bin %q, want /custom/chrome", got)
	}
}

func TestLauncherIsPureConfiguration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created")

	Launcher(Options{ProfileDir: dir, Headless: true})

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("Launcher must not create directories; that is Open's job")
	}
}

func TestInUseFollowsTheSingletonLock(t *testing.T) {
	dir := t.TempDir()

	if InUse(dir) {
		t.Fatal("a fresh profile must not report as in use")
	}

	// Chrome writes this as a symlink to host-pid; its target is irrelevant to us.
	if err := os.Symlink("host-1234", filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if !InUse(dir) {
		t.Fatal("expected the profile to report as in use")
	}
}

// A dangling symlink still means "held" -- Lstat must not follow it, or a lock
// pointing at a dead process would read as free and let two Chromes share the
// profile.
func TestInUseDetectsDanglingLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("/definitely/missing", filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if !InUse(dir) {
		t.Fatal("a dangling lock must still count as in use")
	}
}

func TestClearStaleRemovesLockAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("host-1234", filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := ClearStale(dir); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if InUse(dir) {
		t.Fatal("expected the lock to be gone")
	}
	// Clearing an already-clear profile is a normal startup path, not an error.
	if err := ClearStale(dir); err != nil {
		t.Fatalf("second clear should be a no-op, got %v", err)
	}
}

func TestOpenRefusesAProfileAlreadyInUse(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("host-1234", filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := Open(t.Context(), Options{
		ChromePath: "/nonexistent/chrome",
		ProfileDir: dir,
		Headless:   true,
	})

	if !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("expected ErrProfileInUse, got %v", err)
	}
}

func TestIsTargetLostMatchesOnlyTheTransientError(t *testing.T) {
	if !isTargetLost(errors.New("{-32000 Inspected target navigated or closed }")) {
		t.Error("expected the session-restore race to be recognised")
	}
	if isTargetLost(errors.New("connection refused")) {
		t.Error("unrelated errors must not be retried")
	}
	if isTargetLost(nil) {
		t.Error("nil is not an error")
	}
}
