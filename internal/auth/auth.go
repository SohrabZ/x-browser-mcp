// Package auth decides whether the local X session is usable, and runs the
// interactive login when it is not.
package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/browser"
	"github.com/SohrabZ/x-browser-mcp/internal/pool"
	"github.com/SohrabZ/x-browser-mcp/internal/xui"
)

// ErrLoginRequired reports that no usable session exists.
var ErrLoginRequired = errors.New("no X session; run start_login and sign in")

// State is the coarse session status callers act on.
type State string

const (
	StateReady      State = "ready"
	StateRequired   State = "login_required"
	StateInProgress State = "login_in_progress"
)

// Status is a point-in-time view of the session.
type Status struct {
	State      State     `json:"state"`
	LoggedIn   bool      `json:"logged_in"`
	CheckedAt  time.Time `json:"checked_at"`
	ProfileDir string    `json:"profile_dir"`
}

// Lease borrows a browser session and returns a function that hands it back.
type Lease func(ctx context.Context) (*browser.Session, func(), error)

// Reserver takes exclusive use of the profile.
//
// Only one Chrome may hold a user-data-dir. The login window needs it for as
// long as the user is signing in, so the reservation is held until that window
// closes rather than released as soon as it opens.
type Reserver interface {
	Reserve(ctx context.Context) (pool.Reservation, error)
}

// LoginLauncher opens a visible browser for the user to sign in with. It
// returns the running process so its exit can be observed.
type LoginLauncher func() (*exec.Cmd, error)

// Options configures a Manager.
type Options struct {
	ProfileDir   string
	StatusTTL    time.Duration
	LoginTimeout time.Duration

	Lease       Lease
	Reserve     Reserver
	LaunchLogin LoginLauncher
}

// Manager owns the session verdict and the login lifecycle.
//
// Verdicts are cached because every uncached check drives a real browser at X,
// and checking more often makes losing the session likelier, not less.
type Manager struct {
	opts Options

	// now is swappable so tests need not sleep.
	now func() time.Time

	mu         sync.Mutex
	cached     *Status
	cachedTill time.Time
	login      *attempt
}

// attempt is an in-flight interactive login.
type attempt struct {
	cmd      *exec.Cmd
	done     <-chan error
	deadline time.Time

	// profile is held for the whole sign-in, so no read can warm a browser on
	// the profile while the login window has it.
	profile pool.Reservation
}

// New builds a Manager.
func New(opts Options) *Manager {
	return &Manager{opts: opts, now: time.Now}
}

// negativeTTL keeps a "not signed in" verdict briefly, so a login that just
// completed is noticed quickly while repeated polling still costs nothing.
const negativeTTL = 30 * time.Second

// Status reports whether the session is usable, using the cache when it can.
func (m *Manager) Status(ctx context.Context) (Status, error) {
	if s, ok := m.loginInProgress(); ok {
		return s, nil
	}
	if s, ok := m.cachedStatus(); ok {
		return s, nil
	}

	status, err := m.probe(ctx)
	if err != nil {
		return Status{}, err
	}
	m.remember(status)
	return status, nil
}

// Require returns ErrLoginRequired unless the session is usable.
func (m *Manager) Require(ctx context.Context) error {
	status, err := m.Status(ctx)
	if err != nil {
		return err
	}
	if !status.LoggedIn {
		return ErrLoginRequired
	}
	return nil
}

// loginInProgress reports an in-flight login without touching the profile.
//
// The login browser holds the profile lock, so probing during it would either
// fail on the lock or race the user mid-sign-in.
//
// This tracks the window, not the deadline. A login that overran its timeout is
// still a login: the window is open and still owns the profile, and reporting
// otherwise would send reads at a directory Chrome is holding.
func (m *Manager) loginInProgress() (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.login == nil {
		return Status{}, false
	}
	return Status{
		State:      StateInProgress,
		CheckedAt:  m.now(),
		ProfileDir: m.opts.ProfileDir,
	}, true
}

func (m *Manager) cachedStatus() (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cached == nil || !m.now().Before(m.cachedTill) {
		return Status{}, false
	}
	return *m.cached, true
}

func (m *Manager) remember(s Status) {
	ttl := negativeTTL
	if s.LoggedIn {
		ttl = m.opts.StatusTTL
	}
	if ttl <= 0 {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	cached := s
	m.cached = &cached
	m.cachedTill = m.now().Add(ttl)
}

// Invalidate drops any cached verdict, so the next Status re-probes.
func (m *Manager) Invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cached = nil
}

// probe opens a browser and asks X whether this session is signed in.
func (m *Manager) probe(ctx context.Context) (Status, error) {
	status := Status{
		State:      StateRequired,
		CheckedAt:  m.now(),
		ProfileDir: m.opts.ProfileDir,
	}

	session, release, err := m.opts.Lease(ctx)
	if err != nil {
		// A locked profile means someone else is using it, not that the session
		// is gone; reporting "login required" would send the user into a
		// needless re-login.
		if errors.Is(err, browser.ErrProfileInUse) {
			return status, err
		}
		return Status{}, err
	}
	defer release()

	page, err := session.Page(ctx)
	if err != nil {
		return Status{}, err
	}
	defer page.Close()

	if err := page.Goto(xui.HomeURL); err != nil {
		return Status{}, err
	}

	signedIn, err := signedIn(session, page)
	if err != nil {
		return Status{}, err
	}
	if signedIn {
		status.State = StateReady
		status.LoggedIn = true
	}
	return status, nil
}

// signedIn checks the rendered page first and falls back to cookies.
//
// The DOM check is authoritative when it succeeds: cookies can be present while
// X still refuses the session. The cookie check covers the case where the page
// has not rendered its navigation yet.
func signedIn(session *browser.Session, page *browser.Page) (bool, error) {
	for _, selector := range []string{xui.SelAccountMenu, xui.SelProfileLink} {
		if page.Has(selector, 3*time.Second) {
			return true, nil
		}
	}

	cookies, err := session.Cookies()
	if err != nil {
		return false, fmt.Errorf("read cookies: %w", err)
	}
	return xui.SignedInCookies(cookies), nil
}

// StartLogin opens a visible browser for the user to sign in with.
//
// It returns the deadline by which the login must be completed. If one is
// already open, its deadline is returned rather than opening a second window
// onto the same profile.
func (m *Manager) StartLogin(ctx context.Context) (time.Time, error) {
	m.Invalidate()

	m.mu.Lock()
	if m.login != nil {
		// A window is already open on this profile; a second one cannot have it.
		deadline := m.login.deadline
		m.mu.Unlock()
		return deadline, nil
	}
	m.mu.Unlock()

	// Take the profile before opening the window, and keep it until the window
	// closes. Releasing it early would let the next read start a second Chrome
	// on the directory the user is signing in to.
	var reservation pool.Reservation
	if m.opts.Reserve != nil {
		reserveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		got, err := m.opts.Reserve.Reserve(reserveCtx)
		cancel()
		if err != nil {
			return time.Time{}, fmt.Errorf("reserve profile for login: %w", err)
		}
		reservation = got
	}

	cmd, err := m.opts.LaunchLogin()
	if err != nil {
		if reservation != nil {
			reservation.Release()
		}
		return time.Time{}, fmt.Errorf("open login browser: %w", err)
	}

	att := &attempt{
		cmd:      cmd,
		done:     waitFor(cmd),
		deadline: m.now().Add(m.opts.LoginTimeout),
		profile:  reservation,
	}

	m.mu.Lock()
	m.login = att
	m.mu.Unlock()

	go m.watch(att)
	return att.deadline, nil
}

// watch clears the in-progress marker as soon as the login browser exits.
//
// Without it, status keeps reporting login_in_progress for the rest of the
// timeout even though the user finished — and callers are told to check status
// before reading. Waiting on the process also reaps it.
func (m *Manager) watch(att *attempt) {
	timer := time.NewTimer(time.Until(att.deadline))
	defer timer.Stop()

	select {
	case <-att.done:
	case <-timer.C:
		// The deadline passed, but that says nothing about whether the user
		// closed the window. Chrome still owns the profile, so the reservation
		// is kept and the wait continues.
		//
		// Killing the window here would be worse than waiting: it would discard
		// a sign-in the user may be part way through. Releasing the profile
		// instead would hand the directory to a read while Chrome still holds
		// it, which is the single-owner invariant this exists to protect.
		slog.Warn("x login is taking longer than the timeout; still waiting for the window to close",
			"deadline", att.deadline)
		<-att.done
	}

	m.mu.Lock()
	if m.login == att {
		m.login = nil
	}
	m.cached = nil
	m.mu.Unlock()

	// The window has actually exited, so reads may have the profile back.
	if att.profile != nil {
		att.profile.Release()
	}
}

func waitFor(cmd *exec.Cmd) <-chan error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done
}
