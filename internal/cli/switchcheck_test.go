//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/supervisor"
)

// TestSwitchCheckPolicy (S-6): a switch to a CLI the safety policy cannot
// reach is refused unless [policy] required = false.
func TestSwitchCheckPolicy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGGY_HOME", filepath.Join(home, ".aggy"))
	for _, k := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		t.Setenv(k, "")
	}
	bin := t.TempDir()
	// A claude whose probe fails has no --settings, so it cannot get the
	// policy. It is not the real CLI: a script that only fails.
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	cwd := t.TempDir()
	cfg := config.Default()
	check := switchCheck(context.Background(), cfg, cwd)
	// S-10: the probe runs in the background; the check says so until it
	// is done, then reports the failure once and forgets it.
	settle := func() error {
		deadline := time.Now().Add(30 * time.Second)
		for {
			err := check(adapter.Claude)
			if !errors.Is(err, supervisor.ErrChecking) || time.Now().After(deadline) {
				return err
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if err := check(adapter.Claude); !errors.Is(err, supervisor.ErrChecking) {
		t.Errorf("first check: %v, want ErrChecking", err)
	}
	if err := settle(); err == nil || !strings.Contains(err.Error(), "safety policy") {
		t.Errorf("required policy: %v, want a policy refusal", err)
	}
	if err := check(adapter.Claude); !errors.Is(err, supervisor.ErrChecking) {
		t.Errorf("after a failure: %v, want a new probe (ErrChecking)", err)
	}
	_ = settle()
	cfg.Policy.Required = false
	if err := switchCheck(context.Background(), cfg, cwd)(adapter.Claude); err != nil {
		t.Errorf("policy not required: %v", err)
	}
	if err := switchCheck(context.Background(), cfg, cwd)(adapter.Grok); err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Errorf("grok not on PATH: %v", err)
	}
}

// TestRefreshed (O56-6): reads run in the background; until the first is
// done the default is given.
func TestRefreshed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	get := refreshed(ctx, []adapter.Vendor{adapter.Grok}, func(adapter.Vendor) bool { <-release; return false }, true)
	if !get(adapter.Grok) {
		t.Error("the default was not given while the read ran")
	}
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for get(adapter.Grok) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if get(adapter.Grok) {
		t.Error("the read never landed")
	}
}
