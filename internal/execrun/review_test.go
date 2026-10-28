package execrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
)

// Regression tests for the PR #4 review (C14, S6, S7, S12) and re-review
// (L2), moved here from internal/probe with the runner.

// TestRunnerCallerCancel (C14): the caller cancelling is not a timeout.
func TestRunnerCallerCancel(t *testing.T) {
	f := setup(t)
	run := f.runner(t, `{"sleep_ms":5000}`, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	_, err := run(ctx, adapter.Command{Path: f.fakes.Claude, Args: []string{"-p", "x"}})
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrTimeout) {
		t.Errorf("run = %v, want context.Canceled and not ErrTimeout", err)
	}
}

// TestRunnerPrivateDir (S6): with no Dir the CLI runs in a fresh private
// directory that is removed afterwards.
func TestRunnerPrivateDir(t *testing.T) {
	f := setup(t)
	run := f.runner(t, "", 10*time.Second)
	out, err := run(context.Background(), adapter.Command{Path: f.fakes.Grok, Args: []string{"-p", "x"}})
	if err != nil {
		t.Fatal(err)
	}
	recs := fakecli.Records(t, f.record)
	if len(recs) != 1 {
		t.Fatalf("records = %v (exit %d, stderr %q)", recs, out.ExitCode, out.Stderr)
	}
	cwd := recs[0].Cwd
	tmp, _ := filepath.EvalSymlinks(os.TempDir())
	if cwd == tmp || cwd == os.TempDir() || !strings.Contains(filepath.Base(cwd), "aggy-probe-") {
		t.Errorf("cwd = %s, want a private aggy-probe- dir", cwd)
	}
	if _, err := os.Stat(cwd); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("private dir left behind: %v", err)
	}
}

// TestEnvRules (S6, S7).
func TestEnvRules(t *testing.T) {
	base := []string{"HOME=/h", "PATH=:.:rel/bin:/usr/bin::/bin", "TMPDIR=/t", "USER=u", "SECRET=s"}
	env, err := Env(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(env, "PATH=/usr/bin:/bin") {
		t.Errorf("PATH not cleaned: %v", env)
	}
	if slices.Contains(env, "SECRET=s") {
		t.Error("an unlisted variable passed")
	}
	denied := []string{
		"OPENAI_API_KEY", "ANTHROPIC_BASE_URL", "OPENAI_API_BASE", "ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_OAUTH_TOKEN", "GITHUB_TOKEN", "HTTPS_PROXY", "http_proxy", "ALL_PROXY",
		"NODE_OPTIONS", "node_options", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
		"HOME", "PATH", "TMPDIR", "USER", "home", "CODEX_HOME",
	}
	for _, k := range denied {
		if _, err := Env(base, []string{k + "=x"}); err == nil {
			t.Errorf("%s accepted in a command's env", k)
		}
	}
	for _, k := range []string{"AGGY_DELEGATED", "AGGY_VENDOR"} {
		if _, err := Env(base, []string{k + "=x"}); err != nil {
			t.Errorf("%s refused: %v", k, err)
		}
	}
}

// TestRunnerKillsProcessGroup (S12): a CLI's own child dies with it at the
// timeout.
func TestRunnerKillsProcessGroup(t *testing.T) {
	f := setup(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// The hook is the fake's child: it records its pid and sleeps.
	sc := `{"hooks":[{"event":"Stop","payload":{}}],
		"hook_command":["/bin/sh","-c","echo $$ > ` + pidFile + `; exec sleep 30","sh"]}`
	run := f.runner(t, sc, 2*time.Second)
	_, err := run(context.Background(), adapter.Command{Path: f.fakes.Claude, Args: []string{"-p", "x"}})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("run = %v, want ErrTimeout", err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the grandchild never started: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d outlived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestEnvAllowlist (L2): a command may add only AGGY_* variables. Each of
// these passed the old deny list.
func TestEnvAllowlist(t *testing.T) {
	base := []string{"HOME=/h", "PATH=/usr/bin"}
	for _, k := range []string{
		"BASH_ENV", "ENV", "NODE_PATH", "GIT_SSH_COMMAND", "GIT_CONFIG_GLOBAL", "PYTHONPATH",
		"PERL5OPT", "RUBYOPT", "NODE_EXTRA_CA_CERTS", "SSL_CERT_FILE", "npm_config_registry",
		"ELECTRON_RUN_AS_NODE", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME", "XDG_CONFIG_HOME",
		"aggy_delegated",
	} {
		if _, err := Env(base, []string{k + "=x"}); err == nil {
			t.Errorf("%s accepted in a command's env", k)
		}
	}
	env, err := Env(base, []string{"AGGY_DELEGATED=1", "AGGY_FAKE_SCENARIO=/s"})
	if err != nil || !slices.Contains(env, "AGGY_DELEGATED=1") {
		t.Errorf("AGGY_* refused: %v, %v", env, err)
	}
}

// TestEnvRelativePathUnset (L2): when every PATH entry is relative, PATH is
// left unset rather than set empty.
func TestEnvRelativePathUnset(t *testing.T) {
	env, err := Env([]string{"HOME=/h", "PATH=.:bin::rel/x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			t.Errorf("PATH set to %q, want unset", kv)
		}
	}
}

// TestEnvNarrowAllowlist (T6623): only the named AGGY_ variables and the
// fakes' AGGY_FAKE_* may be added; AGGY_HOME and other AGGY_ names may
// not, and AGGY_DELEGATED from aggy's own environment never passes.
func TestEnvNarrowAllowlist(t *testing.T) {
	base := []string{"HOME=/h", "PATH=/usr/bin", "AGGY_DELEGATED=1", "AGGY_HOME=/s"}
	tests := []struct {
		key string
		ok  bool
	}{
		{"AGGY_DELEGATED", true}, {"AGGY_VENDOR", true}, {"AGGY_MODEL", true},
		{"AGGY_FAKE_SCENARIO", true}, {"AGGY_FAKE_RECORD", true},
		{"AGGY_HOME", false}, {"AGGY_ANYTHING", false}, {"AGGY_FAKE_", false}, {"AGGY_FAKE_x", false},
	}
	for _, tt := range tests {
		if _, err := Env(base, []string{tt.key + "=1"}); (err == nil) != tt.ok {
			t.Errorf("%s: err = %v, want allowed %v", tt.key, err, tt.ok)
		}
	}
	env, err := Env(base, nil)
	if err != nil || slices.Contains(env, "AGGY_DELEGATED=1") || slices.Contains(env, "AGGY_HOME=/s") {
		t.Errorf("inherited AGGY_ variables passed: %v, %v", env, err)
	}
}
