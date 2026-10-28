//go:build unix

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
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
	if err := switchCheck(context.Background(), cfg, cwd)(adapter.Claude); err == nil || !strings.Contains(err.Error(), "safety policy") {
		t.Errorf("required policy: %v, want a policy refusal", err)
	}
	cfg.Policy.Required = false
	if err := switchCheck(context.Background(), cfg, cwd)(adapter.Claude); err != nil {
		t.Errorf("policy not required: %v", err)
	}
	if err := switchCheck(context.Background(), cfg, cwd)(adapter.Grok); err == nil || !strings.Contains(err.Error(), "not on PATH") {
		t.Errorf("grok not on PATH: %v", err)
	}
}
