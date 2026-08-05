// Package browser owns the Chrome lifecycle: how it is launched, how the
// persistent profile is guarded, and how pages are opened against it.
//
// Everything Chrome-shaped lives here so the rest of the project can be written
// and tested without a browser present.
package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
)

// ErrProfileInUse reports that another Chrome already holds the profile. It is
// a sentinel so callers can branch on it instead of matching error text.
var ErrProfileInUse = errors.New("chrome profile is already in use")

// Options describes how to start Chrome.
type Options struct {
	ChromePath  string
	ProfileDir  string // --user-data-dir
	ProfileName string // --profile-directory
	Headless    bool
}

// Launcher builds the Chrome launcher.
//
// It is pure configuration: no directories are created and no process is
// started, so the resulting flags can be asserted on in tests. Opening a
// browser is Open's job.
func Launcher(opts Options) *launcher.Launcher {
	l := launcher.New().Headless(opts.Headless)

	// rod's defaults include --use-mock-keychain, which swaps the macOS login
	// Keychain for a fake one. Chrome then cannot decrypt cookies that a real
	// Chrome wrote, so a logged-in profile reads back as an empty cookie jar --
	// and on exit Chrome re-encrypts the store with the mock key, destroying the
	// saved session. Reusing a persistent profile does not work while it is set.
	l.Delete(mockKeychainFlag)

	if opts.ChromePath != "" {
		l.Bin(opts.ChromePath)
	}
	if opts.ProfileDir != "" {
		l.Set(flags.UserDataDir, opts.ProfileDir)
		if opts.ProfileName != "" {
			l.Set(flags.Flag(profileDirFlag), opts.ProfileName)
		}
	}
	return l
}

// Chrome flag names we depend on by string. Naming them keeps the dependency
// visible and greppable rather than buried in call sites.
const (
	mockKeychainFlag = "use-mock-keychain"
	profileDirFlag   = "profile-directory"
)

// Session is a running Chrome instance and its rod connection.
type Session struct {
	browser *rod.Browser
	l       *launcher.Launcher

	// persistent marks a session running against the user's own profile, which
	// must outlive the browser. Anything else runs in a throwaway directory rod
	// created and is safe to delete on close.
	persistent bool

	// profileDir is retained so Close can confirm the profile was actually
	// released, rather than assuming it.
	profileDir string

	// release is called once on Close, so a caller holding the profile can be
	// unblocked without this package knowing why it was held.
	release func()
}

// Open starts Chrome and connects to it. It returns only once the launch has
// resolved, one way or the other.
//
// There is deliberately no way to give up on a launch early. An abandoned
// launch is worse than a slow one: the Chrome it started still arrives, still
// takes the profile, and nothing is tracking it -- which is exactly the
// duplicate-browser failure the caller was trying to avoid. Callers that cannot
// wait should stop waiting on their own; ownership stays with whoever started
// the launch until it finishes.
//
// When a persistent profile is configured it is created if missing and checked
// for a competing Chrome first; Open fails with ErrProfileInUse rather than
// letting two instances corrupt each other's state.
func Open(ctx context.Context, opts Options) (*Session, error) {
	persistent := opts.ProfileDir != ""

	if persistent {
		if err := os.MkdirAll(opts.ProfileDir, 0o700); err != nil {
			return nil, fmt.Errorf("create profile dir: %w", err)
		}
		if InUse(opts.ProfileDir) {
			return nil, fmt.Errorf("%w (%s)", ErrProfileInUse, lockPath(opts.ProfileDir))
		}
	}

	// Bound the launch itself. Chrome can start and then never publish its
	// debugging URL, and an unbounded wait here would hold the caller's claim on
	// the profile forever -- nothing else could read, write, or sign in.
	launchCtx, cancelLaunch := context.WithTimeout(ctx, launchWait)
	defer cancelLaunch()

	l := Launcher(opts).Context(launchCtx)

	controlURL, err := l.Launch()
	if err != nil {
		// Chrome may have started before the failure, so tear down rather than
		// assume nothing is holding the directory.
		reap(l, opts, persistent)
		return nil, fmt.Errorf("launch chrome: %w", err)
	}

	b := rod.New().ControlURL(controlURL).Context(ctx)
	if err := b.Connect(); err != nil {
		// Chrome is running at this point; it owns the profile until it exits.
		// Returning before that would let the next caller start a second one.
		reap(l, opts, persistent)
		return nil, fmt.Errorf("connect to chrome: %w", err)
	}

	return &Session{browser: b, l: l, persistent: persistent, profileDir: opts.ProfileDir}, nil
}

// ProfileDir reports the persistent profile this session runs against, or ""
// for a throwaway one. Callers use it to confirm the profile is free after a
// shutdown they could not otherwise verify.
func (s *Session) ProfileDir() string {
	if s == nil {
		return ""
	}
	return s.profileDir
}

// OnClose registers a callback run once when the session closes.
func (s *Session) OnClose(fn func()) {
	s.release = fn
}

// Page opens a blank page whose operations are bound to ctx.
//
// A shared browser lives for the life of the server, and pages inherit their
// browser's context, so without rebinding here a caller's deadline would never
// reach Goto, WaitLoad or Eval -- a stalled navigation would hold its lease
// indefinitely and block anything waiting to reserve the profile.
//
// Closing deliberately uses the browser's context rather than ctx: cleanup has
// to succeed after the caller has given up, or abandoned tabs accumulate in a
// browser that is never restarted.
func (s *Session) Page(ctx context.Context) (*Page, error) {
	p, err := s.browser.Page(proto.TargetCreateTarget{URL: "about:blank"})
	if err != nil {
		return nil, fmt.Errorf("open page: %w", err)
	}
	return &Page{page: p.Context(ctx), closer: p}, nil
}

// Alive reports whether the browser is still reachable.
//
// Chrome can go away without this process being told -- a crash, an OOM kill,
// or the user quitting it -- after which every call fails with a closed
// connection. A pooled session is checked before reuse so a dead browser is
// replaced rather than handed out repeatedly.
func (s *Session) Alive(ctx context.Context) bool {
	if s == nil || s.browser == nil {
		return false
	}
	// Any round trip proves the connection is live; the page list is the
	// cheapest one. It is bound to ctx because a Chrome that is running but
	// whose CDP connection has wedged would otherwise never answer, and this
	// probe sits in front of every read.
	_, err := s.browser.Context(ctx).Pages()
	return err == nil
}

// Cookies returns every cookie the browser currently holds.
func (s *Session) Cookies() ([]*proto.NetworkCookie, error) {
	return s.browser.GetCookies()
}

// launchWait bounds a browser launch. Chrome normally publishes its debugging
// URL in well under a second; this only stops a wedged start from holding the
// profile claim indefinitely. It is a variable so tests can shorten it.
var launchWait = 90 * time.Second

// reap shuts down a browser that failed partway through startup and waits for
// it to let go of the profile, so the next attempt is not racing a corpse.
func reap(l *launcher.Launcher, opts Options, persistent bool) {
	pid := l.PID()
	l.Kill()
	exited := waitForExit(pid, exitWait)

	if !persistent {
		go l.Cleanup()
		return
	}
	if opts.ProfileDir == "" {
		return
	}
	if exited {
		clearLockOwnedBy(opts.ProfileDir, pid)
		return
	}
	// The startup failed but Chrome outlived the kill, so it still owns the
	// directory. Record that in the lock rather than returning a plain launch
	// error and letting the next caller find an unclaimed profile: Chrome
	// writes its lock early, but "early" is not "before we gave up on it".
	claimLock(opts.ProfileDir, pid)
}

// claimLock makes the profile lock name pid when nothing else has claimed it,
// so InUse reports the profile held for as long as that process lives -- and
// stale, not held, once it dies.
func claimLock(profileDir string, pid int) {
	if pid <= 0 {
		return
	}
	if _, known := lockOwner(profileDir); known {
		return // someone already owns it; do not overwrite the record
	}
	host, err := os.Hostname()
	if err != nil {
		host = "localhost"
	}
	_ = os.Symlink(fmt.Sprintf("%s-%d", host, pid), lockPath(profileDir))
}

// exitWait bounds how long Close waits for Chrome to actually go away. Chrome
// normally exits in well under a second; this only has to stop a wedged process
// from blocking the caller forever.
const exitWait = 10 * time.Second

// Close shuts down Chrome and does not return until the profile is free.
//
// rod's Kill signals the process and returns immediately, so Close completing
// is not by itself proof that Chrome let go. Anything that takes the profile
// next -- a login window, a write, a replacement read -- would then be starting
// against a directory the old process still holds. Close therefore waits for
// the process to exit, and clears a lock file that a killed Chrome left behind.
func (s *Session) Close() error {
	var unconfirmed error

	if s.browser != nil {
		_ = s.browser.Close()
	}

	if s.l != nil {
		pid := s.l.PID()
		s.l.Kill()
		exited := waitForExit(pid, exitWait)

		// rod always sets a user-data-dir -- its own temp one when we supply no
		// profile -- so ownership is tracked explicitly rather than inferred from
		// that flag being empty, which it never is.
		if !s.persistent {
			go s.l.Cleanup()
		}

		// A Chrome that was killed rather than quit leaves its lock behind, and
		// leaving it would block every later launch with a profile-in-use error.
		//
		// Only a lock naming the process we just watched die is removed. A wedged
		// Chrome that outlived the wait still owns the profile, and deleting its
		// lock would invite a second browser onto the directory -- far worse than
		// the profile-in-use error the leftover lock produces.
		if exited {
			if s.persistent && s.profileDir != "" {
				clearLockOwnedBy(s.profileDir, pid)
			}
		} else {
			if s.persistent && s.profileDir != "" {
				claimLock(s.profileDir, pid)
			}
			unconfirmed = fmt.Errorf("chrome (pid %d) did not exit within %s; it may still hold the profile", pid, exitWait)
		}
	}

	if s.release != nil {
		s.release()
		s.release = nil
	}
	return unconfirmed
}

// waitForExit blocks until the process is gone, reporting whether that was
// confirmed rather than merely waited out.
//
// The distinction matters: callers use it to decide whether the profile is
// genuinely free, and treating a timeout as success would hand the directory to
// a second browser.
func waitForExit(pid int, budget time.Duration) bool {
	if pid <= 0 {
		return true
	}
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return true // gone
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// clearLockOwnedBy removes a SingletonLock only if it names the given process.
//
// Chrome writes the lock as a symlink to "host-pid". Checking the pid keeps a
// shutdown from deleting a lock that some other browser has since taken, which
// would put two of them on one profile.
func clearLockOwnedBy(profileDir string, pid int) {
	owner, known := lockOwner(profileDir)
	if !known {
		return // no lock
	}
	// Only a lock we can positively attribute to the process we just watched
	// die. An owner of zero means the lock is unreadable or was written by
	// another host -- in both cases it is someone's claim and not ours to
	// remove, and removing it would invite a second Chrome onto the profile.
	if owner != pid {
		return
	}
	_ = ClearStale(profileDir)
}

// Page is a browser tab with the navigation behavior X requires.
type Page struct {
	// page carries the caller's context, so their deadline interrupts work.
	page *rod.Page
	// closer carries the browser's context, so cleanup still runs once the
	// caller's context is done.
	closer *rod.Page
}

// Rod exposes the underlying page for callers that need raw access, such as
// evaluating extraction scripts.
func (p *Page) Rod() *rod.Page { return p.page }

// Goto navigates to url and waits for the load event.
//
// The first automated page load after a manual login can lose its target to
// Chrome restoring the previous session's tabs, which CDP reports as "Inspected
// target navigated or closed". That is transient, so one retry is enough.
func (p *Page) Goto(url string) error {
	err := p.navigate(url)
	if isTargetLost(err) {
		err = p.navigate(url)
	}
	return err
}

func (p *Page) navigate(url string) error {
	if err := p.page.Navigate(url); err != nil {
		return err
	}
	return p.page.WaitLoad()
}

// Has reports whether an element matching selector is present, waiting briefly
// for it to render. A missing element is an answer, not an error.
func (p *Page) Has(selector string, wait time.Duration) bool {
	timed := p.page.Timeout(wait)
	defer timed.CancelTimeout()

	el, err := timed.Element(selector)
	return err == nil && el != nil
}

// Close closes the tab, using the browser's context so it still works after the
// caller's deadline has passed.
func (p *Page) Close() {
	target := p.closer
	if target == nil {
		target = p.page
	}
	_ = target.Close()
}

func isTargetLost(err error) bool {
	if err == nil {
		return false
	}
	// rod surfaces this as a CDP error string rather than a typed error, so a
	// substring match is the only handle available.
	return strings.Contains(err.Error(), "Inspected target navigated or closed")
}

// InUse reports whether a Chrome instance currently holds the profile.
//
// This is the single authority on that question. Everything else in the
// project -- a pool retiring a session, a write handing the profile back, a
// login window taking it -- used to answer it from its own bookkeeping, and
// bookkeeping is a cache: every place it was kept was a place it could be
// dropped, and dropping it puts two browsers on one directory.
//
// The directory itself cannot be out of date. Chrome writes SingletonLock as a
// symlink to "host-pid" on startup and removes it on a clean quit; a killed
// Chrome leaves it behind. So the lock existing is not ownership -- ownership
// is the process it names still being alive. A lock naming a dead process is
// stale and the profile is free.
//
// An unreadable lock, or one issued by another host, is treated as in use.
// That is the safe direction throughout: refusing to start is recoverable and
// self-clearing, while two Chromes on one profile corrupts it. The same applies
// to the one way this can be wrong -- an exited Chrome whose pid has since been
// reused -- which reads as held until that process ends.
func InUse(profileDir string) bool {
	pid, known := lockOwner(profileDir)
	switch {
	case !known:
		return false // no lock at all
	case pid <= 0:
		return true // a lock we cannot read: assume someone holds it
	default:
		return alive(pid)
	}
}

// lockOwner reports the process named by the profile lock. known is false when
// there is no lock; pid is zero when there is one but it cannot be read as a
// process on this machine.
//
// The lock names a host as well as a pid, and a pid only means something on the
// host that issued it. A profile on shared storage can be held by a Chrome
// elsewhere, and testing that pid locally would answer a question nobody asked.
func lockOwner(profileDir string) (pid int, known bool) {
	target, err := os.Readlink(lockPath(profileDir))
	if err != nil {
		if _, statErr := os.Lstat(lockPath(profileDir)); statErr == nil {
			return 0, true // present, but not a symlink we understand
		}
		return 0, false
	}
	i := strings.LastIndex(target, "-")
	if i < 0 {
		return 0, true
	}
	if host, err := os.Hostname(); err == nil && target[:i] != host {
		return 0, true // another machine's claim: not ours to judge
	}
	owner, err := strconv.Atoi(target[i+1:])
	if err != nil || owner <= 0 {
		return 0, true
	}
	return owner, true
}

func alive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, signal 0 tests for existence without delivering anything.
	return proc.Signal(syscall.Signal(0)) == nil
}

// WaitUntilFree blocks until no Chrome holds the profile.
//
// Callers that have just shut a browser down use this to confirm the handoff
// instead of trusting that Close did what it was asked. It returns an error if
// the profile is still held when the budget runs out, so an unconfirmed
// shutdown stays unconfirmed rather than quietly becoming a success.
func WaitUntilFree(ctx context.Context, profileDir string, budget time.Duration) error {
	if profileDir == "" {
		return nil
	}
	deadline := time.Now().Add(budget)
	for {
		if !InUse(profileDir) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w (%s) after %s", ErrProfileInUse, lockPath(profileDir), budget)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// ClearStale removes a leftover profile lock. Callers must confirm no Chrome is
// running against the profile first; removing a live lock invites two instances
// onto the same profile.
func ClearStale(profileDir string) error {
	err := os.Remove(lockPath(profileDir))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func lockPath(profileDir string) string {
	return filepath.Join(profileDir, "SingletonLock")
}

// ChromePathForTest exposes Chrome discovery to tests in this package's suite
// that need a real browser and should skip when none is installed.
func ChromePathForTest() string {
	for _, c := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
	} {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}
