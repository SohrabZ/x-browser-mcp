// Package auth decides whether the local X session is usable, and runs the
// interactive login when it is not.
package auth

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/browser"
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

// Opener starts a browser session against the persistent profile.
type Opener func(ctx context.Context, headless bool) (*browser.Session, error)

// LoginLauncher opens a visible browser for the user to sign in with. It
// returns the running process so its exit can be observed.
type LoginLauncher func() (*exec.Cmd, error)

// Options configures a Manager.
type Options struct {
	ProfileDir   string
	StatusTTL    time.Duration
	LoginTimeout time.Duration

	Open        Opener
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
func (m *Manager) loginInProgress() (Status, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.login == nil || !m.now().Before(m.login.deadline) {
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

	session, err := m.opts.Open(ctx, true)
	if err != nil {
		// A locked profile means someone else is using it, not that the session
		// is gone; reporting "login required" would send the user into a
		// needless re-login.
		if errors.Is(err, browser.ErrProfileInUse) {
			return status, err
		}
		return Status{}, err
	}
	defer session.Close()

	page, err := session.Page()
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
	if m.login != nil && m.now().Before(m.login.deadline) {
		deadline := m.login.deadline
		m.mu.Unlock()
		return deadline, nil
	}
	m.mu.Unlock()

	cmd, err := m.opts.LaunchLogin()
	if err != nil {
		return time.Time{}, fmt.Errorf("open login browser: %w", err)
	}

	att := &attempt{
		cmd:      cmd,
		done:     waitFor(cmd),
		deadline: m.now().Add(m.opts.LoginTimeout),
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
	}

	m.mu.Lock()
	if m.login == att {
		m.login = nil
	}
	m.cached = nil
	m.mu.Unlock()
}

func waitFor(cmd *exec.Cmd) <-chan error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	return done
}
