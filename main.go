// Command x-browser-mcp serves the local X session to MCP clients and over HTTP.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/SohrabZ/x-browser-mcp/internal/auth"
	"github.com/SohrabZ/x-browser-mcp/internal/browser"
	"github.com/SohrabZ/x-browser-mcp/internal/config"
	"github.com/SohrabZ/x-browser-mcp/internal/httpapi"
	"github.com/SohrabZ/x-browser-mcp/internal/limit"
	"github.com/SohrabZ/x-browser-mcp/internal/mcpapi"
	"github.com/SohrabZ/x-browser-mcp/internal/pool"
	"github.com/SohrabZ/x-browser-mcp/internal/read"
	"github.com/SohrabZ/x-browser-mcp/internal/write"
	"github.com/SohrabZ/x-browser-mcp/internal/xui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Default()

	flag.StringVar(&cfg.ListenAddr, "addr", cfg.ListenAddr, "HTTP listen address")
	flag.StringVar(&cfg.StateDir, "state-dir", cfg.StateDir, "directory holding the browser profile and audit log")
	flag.StringVar(&cfg.ChromePath, "chrome", cfg.ChromePath, "path to the Chrome binary")
	flag.StringVar(&cfg.ProfileName, "profile", cfg.ProfileName, "Chrome profile directory inside the profile dir")
	flag.BoolVar(&cfg.Headless, "headless", cfg.Headless, "run read browsers headless")
	flag.BoolVar(&cfg.AllowWrites, "allow-writes", cfg.AllowWrites, "enable the write tools (post, reply, like, repost, bookmark)")
	flag.DurationVar(&cfg.FetchTimeout, "fetch-timeout", cfg.FetchTimeout, "time budget for a single read")
	flag.DurationVar(&cfg.LoginTimeout, "login-timeout", cfg.LoginTimeout, "how long an interactive login may stay open")
	flag.DurationVar(&cfg.BrowserIdle, "browser-idle", cfg.BrowserIdle, "how long a browser stays warm between reads (0 disables warming)")

	// Pacing is exposed because the right values depend on how hard you drive
	// the server. Raising them raises the odds X flags the session.
	flag.DurationVar(&cfg.ReadPace.MinInterval, "read-interval", cfg.ReadPace.MinInterval, "minimum gap between live reads")
	flag.DurationVar(&cfg.ReadPace.Window, "read-window", cfg.ReadPace.Window, "rolling window for the read budget")
	flag.IntVar(&cfg.ReadPace.Max, "read-max", cfg.ReadPace.Max, "maximum live reads per read-window")
	flag.DurationVar(&cfg.WritePace.MinInterval, "write-interval", cfg.WritePace.MinInterval, "minimum gap between writes")
	flag.DurationVar(&cfg.WritePace.Window, "write-window", cfg.WritePace.Window, "rolling window for the write budget")
	flag.IntVar(&cfg.WritePace.Max, "write-max", cfg.WritePace.Max, "maximum writes per write-window")
	flag.DurationVar(&cfg.WritePace.Jitter, "write-jitter", cfg.WritePace.Jitter, "random extra delay added to the gap between writes")
	flag.Parse()

	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.EnsureStateDir(); err != nil {
		return fmt.Errorf("prepare state dir: %w", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// A Chrome killed rather than quit leaves its lock behind, and every later
	// read then fails with "profile is already in use". Clearing it at startup
	// is safe only because nothing else is running against the profile yet.
	if !browser.InUse(cfg.ProfileDir()) {
		_ = browser.ClearStale(cfg.ProfileDir())
	}

	open := browserOpener(cfg)

	// One warm browser is shared by every read. Launching and quitting Chrome
	// costs about 1.8s per read on its own, and a cold browser loads pages far
	// more slowly, so reuse is worth roughly 3.2s of a 4.5s read.
	//
	// The browser is opened against a server-lifetime context, never the
	// context of whichever request happened to warm it. rod ties a browser's
	// life to the context it was built with, so using a request context here
	// killed the shared browser the moment that first request returned and
	// every later lease failed with "context canceled".
	browserCtx, closeBrowsers := context.WithCancel(context.Background())
	defer closeBrowsers()

	browsers := pool.New(func(context.Context) (pool.Session, error) {
		// The browser lives as long as the server, never as long as the request
		// that happened to warm it. An impatient caller stops waiting; the pool
		// keeps tracking the launch either way.
		return browser.Open(browserCtx, browser.Options{
			ChromePath:  cfg.ChromePath,
			ProfileDir:  cfg.ProfileDir(),
			ProfileName: cfg.ProfileName,
			Headless:    cfg.Headless,
		})
	}, cfg.BrowserIdle)
	defer browsers.Close()

	lease := func(ctx context.Context) (*browser.Session, func(), error) {
		l, err := browsers.Acquire(ctx)
		if err != nil {
			return nil, nil, err
		}
		return l.Session.(*browser.Session), l.Release, nil
	}

	authManager := auth.New(auth.Options{
		ProfileDir:   cfg.ProfileDir(),
		StatusTTL:    cfg.StatusTTL,
		LoginTimeout: cfg.LoginTimeout,
		Lease:        lease,
		Reserve:      browsers,
		LaunchLogin:  loginLauncher(cfg),
	})

	reader := read.New(read.Options{
		Lease:    lease,
		Auth:     authManager,
		Budget:   limit.New(pace(cfg.ReadPace)),
		CacheFor: cfg.ResultTTL,
		Timeout:  cfg.FetchTimeout,
	})

	gate, err := write.NewGate(cfg.AllowWrites)
	if err != nil {
		return err
	}
	writer := write.New(write.Options{
		Open:    open,
		Auth:    authManager,
		Gate:    gate,
		Budget:  limit.New(pace(cfg.WritePace)),
		Audit:   write.NewAuditor(cfg.AuditLogPath()),
		Reserve: browsers,
		Timeout: cfg.FetchTimeout,

		// A successful write changes what the next read should return.
		OnChange: reader.Invalidate,
	})

	handler := httpapi.Handler(httpapi.Deps{
		Auth:   authManager,
		Reader: reader,
		MCP:    mcpapi.Server(mcpapi.Deps{Auth: authManager, Reader: reader, Writer: writer}),
		Log:    log,
	})

	srv := httpapi.Server(cfg.ListenAddr, handler)

	if banner := gate.Banner(); banner != "" {
		fmt.Fprintln(os.Stderr, banner)
	}
	if !cfg.LoopbackOnly() {
		log.Warn("listening beyond loopback; the API is unauthenticated and exposes your X session",
			"addr", cfg.ListenAddr)
	}
	log.Info("x-browser-mcp listening", "addr", cfg.ListenAddr, "profile", cfg.ProfileDir(), "writes", cfg.AllowWrites)

	return serve(srv, log)
}

// serve runs the server until interrupted, then shuts it down gracefully.
func serve(srv *http.Server, log *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case <-quit:
		log.Info("shutting down")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// pace converts a configured pace into limiter parameters.
func pace(p config.Pace) limit.Params {
	return limit.Params{
		MinInterval: p.MinInterval,
		Window:      p.Window,
		Max:         p.Max,
		Jitter:      p.Jitter,
	}
}

// browserOpener returns an opener bound to the configured profile.
func browserOpener(cfg config.Config) func(context.Context, bool) (*browser.Session, error) {
	return func(ctx context.Context, headless bool) (*browser.Session, error) {
		return browser.Open(ctx, browser.Options{
			ChromePath:  cfg.ChromePath,
			ProfileDir:  cfg.ProfileDir(),
			ProfileName: cfg.ProfileName,
			// A caller asking for a visible window always gets one; the config
			// flag only decides whether reads may run headless.
			Headless: headless && cfg.Headless,
		})
	}
}

// loginLauncher opens Chrome directly rather than through rod.
//
// The sign-in window belongs to the user, not to automation: it must stay open
// while they type, outlive this process's page handles, and be quit by hand so
// Chrome flushes cookies and releases the profile lock.
func loginLauncher(cfg config.Config) func() (*exec.Cmd, error) {
	return func() (*exec.Cmd, error) {
		// Everything else reaches Chrome through browser.Open, which refuses a
		// profile someone already owns. This path does not, so it makes the
		// same check: a reservation coordinates callers, but the directory is
		// what decides, and a second Chrome here would corrupt the very profile
		// the user is signing in to.
		if browser.InUse(cfg.ProfileDir()) {
			return nil, fmt.Errorf("%w (%s)", browser.ErrProfileInUse, cfg.ProfileDir())
		}

		args := []string{
			"--user-data-dir=" + cfg.ProfileDir(),
			"--profile-directory=" + cfg.ProfileName,
			"--no-first-run",
			"--no-default-browser-check",
			xui.HomeURL,
		}
		cmd := exec.Command(cfg.ChromePath, args...)
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return cmd, nil
	}
}
