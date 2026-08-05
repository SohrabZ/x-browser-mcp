package browser

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
	"net/http"
	"net/http/httptest"
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

// lockFor writes the profile lock the way Chrome does: this host, that pid.
func lockFor(t *testing.T, dir string, pid int) {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Fatalf("hostname: %v", err)
	}
	if err := os.Symlink(fmt.Sprintf("%s-%d", host, pid), filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
}

func TestInUseFollowsTheSingletonLock(t *testing.T) {
	dir := t.TempDir()

	if InUse(dir) {
		t.Fatal("a fresh profile must not report as in use")
	}

	// Chrome writes this as a symlink to host-pid, and the pid is the whole
	// point: ownership is the named process being alive, not the file existing.
	lockFor(t, dir, os.Getpid())
	if !InUse(dir) {
		t.Fatal("expected the profile to report as in use")
	}
}

// A lock whose target names no pid is held. We cannot tell whether anyone owns
// it, and guessing "free" is the guess that puts two Chromes on one profile.
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
	lockFor(t, dir, os.Getpid())

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
	lockFor(t, dir, os.Getpid())

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

// A leased page must honour the caller's deadline.
//
// The shared browser outlives any request, and pages inherit their browser's
// context, so without rebinding a stalled navigation would ignore the read's
// timeout entirely -- holding its lease and blocking anything waiting to
// reserve the profile. This drives a real browser against a server that never
// responds, which is the shape of that failure.
func TestLeasedPageHonoursCallerDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("needs a browser")
	}
	chrome := ChromePathForTest()
	if chrome == "" {
		t.Skip("no Chrome installed")
	}

	// A listener that accepts and then never replies: any navigation to it hangs.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c // hold it open, send nothing
		}
	}()

	// The browser is on a long-lived context, as the pool keeps it.
	browserCtx, cancelBrowser := context.WithCancel(context.Background())
	defer cancelBrowser()

	sess, err := Open(browserCtx, Options{ChromePath: chrome, Headless: true})
	if err != nil {
		t.Skipf("cannot start chrome: %v", err)
	}
	defer sess.Close()

	// The caller's deadline is short.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	page, err := sess.Page(ctx)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	defer page.Close()

	start := time.Now()
	err = page.Goto("http://" + ln.Addr().String())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the hung navigation to fail once the deadline passed")
	}
	if elapsed > 15*time.Second {
		t.Fatalf("navigation ran for %s; the caller's deadline did not interrupt it", elapsed)
	}
}

// Close must not return until Chrome has actually exited and the profile is
// free. rod's Kill only signals the process, so treating Close as the handoff
// signal would let a login, a write, or a replacement read start against a
// directory the old process still holds.
func TestCloseWaitsForChromeToExitAndFreesTheProfile(t *testing.T) {
	chrome := ChromePathForTest()
	if chrome == "" {
		t.Skip("no Chrome installed")
	}

	dir := t.TempDir()
	sess, err := Open(context.Background(), Options{
		ChromePath: chrome,
		ProfileDir: dir,
		Headless:   true,
	})
	if err != nil {
		t.Skipf("cannot start chrome: %v", err)
	}

	pid := sess.l.PID()
	if pid <= 0 {
		t.Fatal("no browser pid to observe")
	}
	if !InUse(dir) {
		t.Fatal("a running browser should hold the profile lock")
	}

	sess.Close()

	// The process is gone, not merely signalled.
	if proc, err := os.FindProcess(pid); err == nil {
		if err := proc.Signal(syscall.Signal(0)); err == nil {
			t.Error("Close returned while the Chrome process was still alive")
		}
	}
	// And the profile is takeable: this is the invariant callers depend on.
	if InUse(dir) {
		t.Error("Close returned while the profile lock was still held")
	}
	replacement, err := Open(context.Background(), Options{
		ChromePath: chrome, ProfileDir: dir, Headless: true,
	})
	if err != nil {
		t.Fatalf("a replacement could not take the profile after Close: %v", err)
	}
	replacement.Close()
}

// Startup deliberately has no early-abandon path. A launch that is given up on
// still produces a Chrome that takes the profile, with nothing tracking it --
// the duplicate-browser failure the pool exists to prevent. Ownership therefore
// stays with the launch until it resolves, and callers stop waiting on their
// own side.
func TestOpenHasNoEarlyAbandonPath(t *testing.T) {
	// An already-cancelled context must not make Open return before the launch
	// has actually resolved one way or the other.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Open(ctx, Options{ChromePath: "/nonexistent/definitely-not-chrome", Headless: true})
	if err == nil {
		t.Fatal("expected a launch against a missing binary to fail")
	}
	// The failure must come from the launch itself, not from abandoning it.
	if strings.Contains(err.Error(), "deadline") || strings.Contains(err.Error(), "canceled") {
		t.Fatalf("Open abandoned the launch instead of resolving it: %v", err)
	}
}

// A wedged Chrome still owns the profile. Deleting its lock because a wait
// elapsed would invite a second browser onto the directory, which is worse than
// the profile-in-use error the leftover lock produces.
func TestLockOfALiveProcessIsNotCleared(t *testing.T) {
	dir := t.TempDir()

	// A lock naming this test process, which is very much alive.
	lockFor(t, dir, os.Getpid())

	// Some other pid's shutdown must not touch it.
	clearLockOwnedBy(dir, os.Getpid()+1)

	if !InUse(dir) {
		t.Fatal("a lock belonging to another process was deleted")
	}

	// Its own owner's shutdown may.
	clearLockOwnedBy(dir, os.Getpid())
	if InUse(dir) {
		t.Fatal("the owning process should be able to clear its own stale lock")
	}
}

func TestWaitForExitReportsWhetherItConfirmed(t *testing.T) {
	// A process that will not exit within the budget.
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	if waitForExit(cmd.Process.Pid, 100*time.Millisecond) {
		t.Error("a running process must not be reported as exited")
	}

	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()

	if !waitForExit(cmd.Process.Pid, 2*time.Second) {
		t.Error("an exited process should be confirmed gone")
	}
}

// A launch that never publishes a debugging URL must not hold the profile claim
// forever: everything else -- reads, writes, sign-in -- queues behind it.
func TestLaunchIsBounded(t *testing.T) {
	// A "browser" that starts and then does nothing is the shape of the hang.
	script := filepath.Join(t.TempDir(), "wedged.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	original := launchWait
	launchWait = 2 * time.Second // the real bound is minutes; the point is that one exists
	t.Cleanup(func() { launchWait = original })

	began := time.Now()
	_, err := Open(context.Background(), Options{ChromePath: script, Headless: true})
	elapsed := time.Since(began)

	if err == nil {
		t.Fatal("expected a wedged launch to fail")
	}
	if elapsed > 30*time.Second {
		t.Fatalf("a wedged launch ran for %s; it must be bounded", elapsed)
	}
}

// A connect failure leaves Chrome running, and it owns the profile until it
// exits. Returning before that would let the next caller start a second one.
func TestFailedStartupFreesTheProfile(t *testing.T) {
	chrome := ChromePathForTest()
	if chrome == "" {
		t.Skip("no Chrome installed")
	}
	dir := t.TempDir()

	// Start and immediately tear down through the same path a failed startup
	// uses, then confirm the profile is takeable.
	sess, err := Open(context.Background(), Options{ChromePath: chrome, ProfileDir: dir, Headless: true})
	if err != nil {
		t.Skipf("cannot start chrome: %v", err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if InUse(dir) {
		t.Fatal("the profile is still locked after teardown")
	}
	replacement, err := Open(context.Background(), Options{ChromePath: chrome, ProfileDir: dir, Headless: true})
	if err != nil {
		t.Fatalf("a replacement could not take the profile: %v", err)
	}
	_ = replacement.Close()
}

// A lock is not ownership. Chrome leaves one behind when it is killed, and a
// lock naming a process that no longer exists means the profile is free --
// treating it as held would make the service permanently unavailable after one
// crash.
func TestStaleLockDoesNotHoldTheProfile(t *testing.T) {
	dir := t.TempDir()

	// A process that has certainly exited.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	dead := cmd.Process.Pid
	lockFor(t, dir, dead)

	if InUse(dir) {
		t.Fatal("a lock naming a dead process must not hold the profile")
	}
}

// The converse: a lock naming a living process is ownership, and nothing may
// take the profile while that process is alive.
func TestLiveLockHoldsTheProfile(t *testing.T) {
	dir := t.TempDir()

	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	lockFor(t, dir, cmd.Process.Pid)

	if !InUse(dir) {
		t.Fatal("a lock naming a running process must hold the profile")
	}
	if err := WaitUntilFree(context.Background(), dir, 200*time.Millisecond); err == nil {
		t.Fatal("waiting for a held profile must not report it free")
	}

	// Once the owner goes, the profile is free without anyone cleaning up.
	_ = cmd.Process.Kill()
	_ = cmd.Wait() // reap it; a zombie still answers signal 0
	if err := WaitUntilFree(context.Background(), dir, 5*time.Second); err != nil {
		t.Fatalf("the profile should free itself when its owner exits: %v", err)
	}
}

// An unreadable lock is treated as held: refusing to start is recoverable, two
// Chromes on one profile is not.
func TestUnparseableLockIsTreatedAsHeld(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SingletonLock"), []byte("junk"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !InUse(dir) {
		t.Fatal("a lock we cannot read must be assumed to hold the profile")
	}
}

// A startup that failed while Chrome kept running must leave the profile
// recorded as held. Returning a plain launch error would let the next caller
// find an unclaimed directory and start a second browser on it.
func TestSurvivingStartupKeepsTheProfileClaimed(t *testing.T) {
	dir := t.TempDir()

	cmd := exec.Command("sleep", "60") // stands in for a Chrome that outlived the kill
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	claimLock(dir, cmd.Process.Pid)

	if !InUse(dir) {
		t.Fatal("a surviving startup must leave the profile claimed")
	}
	if _, err := Open(context.Background(), Options{ProfileDir: dir, Headless: true}); !errors.Is(err, ErrProfileInUse) {
		t.Fatalf("got %v, want ErrProfileInUse", err)
	}
}

// Claiming must never overwrite a record of someone else's ownership.
func TestClaimDoesNotStealAnExistingLock(t *testing.T) {
	dir := t.TempDir()
	lockFor(t, dir, 4242)
	claimLock(dir, 9999)

	owner, known := lockOwner(dir)
	if !known || owner != 4242 {
		t.Fatalf("got owner %d (known %v); the original claim must stand", owner, known)
	}
}

// A pid only means something on the host that issued it. If the profile lives
// on shared storage, another machine's Chrome may hold it, and testing that pid
// locally answers a question nobody asked.
func TestALockFromAnotherHostIsHeld(t *testing.T) {
	dir := t.TempDir()

	// A pid that is certainly not running here, attributed to another machine.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := os.Symlink(fmt.Sprintf("some-other-host-%d", cmd.Process.Pid), filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if !InUse(dir) {
		t.Fatal("another host's lock must be treated as held, whatever that pid means locally")
	}
}

// Cleanup removes our own leftover lock, never someone else's. A lock we cannot
// attribute -- unreadable, or written by another machine -- is a claim, and
// deleting it is how two Chromes end up on one profile.
func TestCleanupLeavesAnUnattributableLockAlone(t *testing.T) {
	t.Run("another host", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Symlink("some-other-host-4242", filepath.Join(dir, "SingletonLock")); err != nil {
			t.Fatalf("symlink: %v", err)
		}
		clearLockOwnedBy(dir, 4242) // same pid number, different machine
		if !InUse(dir) {
			t.Fatal("another host's lock was removed")
		}
	})

	t.Run("unreadable", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "SingletonLock"), []byte("junk"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		clearLockOwnedBy(dir, 4242)
		if !InUse(dir) {
			t.Fatal("a lock we could not read was removed")
		}
	})

	t.Run("our own", func(t *testing.T) {
		dir := t.TempDir()
		lockFor(t, dir, 4242)
		clearLockOwnedBy(dir, 4242)
		if InUse(dir) {
			t.Fatal("our own leftover lock should be cleared")
		}
	})
}

// A page with unsaved work raises "Leave site?" when it is closed, and then
// waits for an answer. During teardown nobody is there to give one: X registers
// such a hook on its composer, so a write that had just posted left the browser
// parked behind a modal, still holding the profile.
func TestClosingATabDoesNotWaitOnABeforeUnloadPrompt(t *testing.T) {
	chrome := ChromePathForTest()
	if chrome == "" {
		t.Skip("no Chrome installed")
	}

	// A page that insists it has unsaved changes, the way a composer does.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body>composing<script>
			window.addEventListener('beforeunload', function (e) {
				e.preventDefault();
				e.returnValue = 'unsaved';
				return 'unsaved';
			});
		</script></body></html>`))
	}))
	defer srv.Close()

	// Headless Chrome dismisses these dialogs by itself, so a headless run
	// would pass whether or not the bug is present. Writes use a visible
	// browser, which is where the modal actually appeared, so this needs one --
	// and a visible browser needs a display, which CI does not have.
	if os.Getenv("XBM_HEADED_TESTS") == "" {
		t.Skip("set XBM_HEADED_TESTS=1 to run; needs a visible browser")
	}

	session, err := Open(context.Background(), Options{ChromePath: chrome, Headless: false})
	if err != nil {
		t.Skipf("cannot start chrome: %v", err)
	}
	defer func() { _ = session.Close() }()

	page, err := session.Page(context.Background())
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	if err := page.Goto(srv.URL); err != nil {
		t.Fatalf("goto: %v", err)
	}
	// Chrome ignores beforeunload until the user has actually interacted with
	// the page. A write types into the composer and clicks Post, which arms it;
	// without a gesture here the prompt would never fire and this test would
	// pass whether or not the bug is present.
	body, err := page.Rod().Element("body")
	if err != nil {
		t.Fatalf("body: %v", err)
	}
	if err := body.Click(proto.InputMouseButtonLeft, 1); err != nil {
		t.Fatalf("click: %v", err)
	}
	time.Sleep(300 * time.Millisecond)

	closed := make(chan struct{})
	go func() { page.Close(); close(closed) }()

	select {
	case <-closed:
	case <-time.After(15 * time.Second):
		t.Fatal("closing the tab blocked, which is what a beforeunload prompt does")
	}
}
