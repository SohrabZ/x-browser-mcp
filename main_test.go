package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/SohrabZ/x-browser-mcp/internal/browser"
	"github.com/SohrabZ/x-browser-mcp/internal/config"
)

// The sign-in window is the one Chrome this project starts without going
// through browser.Open, so it is the one place the profile guard could be
// missing. A second browser here lands on the profile the user is signing in
// to, which is the copy everything else depends on.
func TestLoginWindowRefusesAProfileSomeoneElseHolds(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "profile")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	holder := exec.Command("sleep", "60")
	if err := holder.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill() })

	host, _ := os.Hostname()
	if err := os.Symlink(fmt.Sprintf("%s-%d", host, holder.Process.Pid), filepath.Join(dir, "SingletonLock")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	cfg := config.Config{StateDir: state, ChromePath: "/nonexistent/chrome"}
	launch := loginLauncher(cfg)

	cmd, err := launch()
	if !errors.Is(err, browser.ErrProfileInUse) {
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		t.Fatalf("got %v, want ErrProfileInUse", err)
	}
}
