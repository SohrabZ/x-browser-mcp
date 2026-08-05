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

	"x-browser-mcp/internal/auth"
	"x-browser-mcp/internal/browser"
	"x-browser-mcp/internal/config"
	"x-browser-mcp/internal/httpapi"
	"x-browser-mcp/internal/limit"
	"x-browser-mcp/internal/mcpapi"
	"x-browser-mcp/internal/read"
	"x-browser-mcp/internal/write"
	"x-browser-mcp/internal/xui"
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

	authManager := auth.New(auth.Options{
		ProfileDir:   cfg.ProfileDir(),
		StatusTTL:    cfg.StatusTTL,
		LoginTimeout: cfg.LoginTimeout,
		Open:         open,
		LaunchLogin:  loginLauncher(cfg),
	})

	reader := read.New(read.Options{
		Open:     open,
		Auth:     authManager,
		Budget:   limit.New(cfg.ReadPace.MinInterval, cfg.ReadPace.Window, cfg.ReadPace.Max),
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
		Budget:  limit.New(cfg.WritePace.MinInterval, cfg.WritePace.Window, cfg.WritePace.Max),
		Audit:   write.NewAuditor(cfg.AuditLogPath()),
		Timeout: cfg.FetchTimeout,
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
