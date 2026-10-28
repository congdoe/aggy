package probe

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
	"github.com/congdoe/aggy/internal/store"
)

// Regression tests for the PR #4 review (C6, C8, C9, C14, S6, S7, S12).
// Each fails on 522a431.

// TestVersionIsAnchored (C6): a version in a notice is not the CLI's, and
// stderr is read when stdout is empty.
func TestVersionIsAnchored(t *testing.T) {
	tests := []struct {
		name           string
		stdout, stderr string
		want           string
	}{
		{"notice before", "A new version 3.0.0 is available\ncodex-cli 0.164.0\n", "", "0.164.0"},
		{"notice after", "grok 1.0.70\nnote: 9.9.9 is the latest", "", "1.0.70"},
		{"stderr only", "", "claude 2.1.311\n", "2.1.311"},
	}
	for _, tt := range tests {
		run := func(context.Context, adapter.Command) (adapter.Output, error) {
			return adapter.Output{Stdout: []byte(tt.stdout), Stderr: []byte(tt.stderr)}, nil
		}
		if got, err := Version(context.Background(), run, "/x/cli", nil); err != nil || got != tt.want {
			t.Errorf("%s: Version = %q, %v, want %q", tt.name, got, err, tt.want)
		}
	}
}

// TestProbeNotCachedAfterHelpFailure (C8): a help run that fails once is
// run again on the next probe, and the full result is then cached.
func TestProbeNotCachedAfterHelpFailure(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	helpRuns := 0
	run := func(_ context.Context, c adapter.Command) (adapter.Output, error) {
		if c.Args[0] == "--version" {
			return adapter.Output{Stdout: []byte("grok 1.0.70\n")}, nil
		}
		helpRuns++
		if helpRuns == 1 {
			return adapter.Output{ExitCode: 1, Stderr: []byte("transient")}, nil
		}
		return adapter.Output{Stdout: []byte("  --session-id <uuid>\n")}, nil
	}
	pr := &Prober{Store: f.store, Run: run}
	if _, _, err := pr.Probe(ctx, adapter.Grok, "/x/grok"); !errors.Is(err, ErrIncomplete) {
		t.Fatalf("first probe = %v, want ErrIncomplete", err)
	}
	p, cached, err := pr.Probe(ctx, adapter.Grok, "/x/grok")
	if err != nil || cached || !Has(p, CapSessionID) {
		t.Fatalf("second probe = %+v, cached %v, %v", p, cached, err)
	}
	if _, cached, _ := pr.Probe(ctx, adapter.Grok, "/x/grok"); !cached || helpRuns != 2 {
		t.Errorf("third probe cached %v after %d help runs, want cached after 2", cached, helpRuns)
	}
}

// TestCodexSubcommandsFromCommandsBlock (C9): exec and resume count only
// as Commands: entries and a working exec resume --help, not as words in
// prose.
func TestCodexSubcommandsFromCommandsBlock(t *testing.T) {
	prose := "Usage: codex [OPTIONS]\n\nYou can exec a prompt and resume it later.\n\nOptions:\n  -c <key=value>\n"
	listed := "Usage: codex [OPTIONS]\n\nCommands:\n  exec    Run non-interactively\n  resume  Resume a session\n\nOptions:\n  -c <key=value>\n"
	for _, tt := range []struct {
		name       string
		top        string
		resumeExit int
		want       []string
	}{
		{"prose only", prose, 2, []string{CapConfigOverride}},
		{"listed", listed, 0, []string{CapHeadless, CapResume, CapConfigOverride}},
	} {
		run := func(_ context.Context, c adapter.Command) (adapter.Output, error) {
			switch strings.Join(c.Args, " ") {
			case "--version":
				return adapter.Output{Stdout: []byte("codex-cli 0.164.0")}, nil
			case "--help":
				return adapter.Output{Stdout: []byte(tt.top)}, nil
			case "exec resume --help":
				return adapter.Output{ExitCode: tt.resumeExit}, nil
			}
			return adapter.Output{ExitCode: 2}, nil
		}
		p, err := Detect(context.Background(), run, adapter.Codex, "/x/codex")
		if err != nil || !slices.Equal(p.Capabilities, tt.want) {
			t.Errorf("%s: caps = %v, %v, want %v", tt.name, p.Capabilities, err, tt.want)
		}
	}
}

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
	if _, err := run(context.Background(), adapter.Command{Path: f.fakes.Grok, Args: []string{"-p"}}); err != nil {
		t.Fatal(err)
	}
	cwd := fakecli.Records(t, f.record)[0].Cwd
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
		"HOME", "PATH", "TMPDIR", "USER", "home",
	}
	for _, k := range denied {
		if _, err := Env(base, []string{k + "=x"}); err == nil {
			t.Errorf("%s accepted in a command's env", k)
		}
	}
	for _, k := range []string{"AGGY_DELEGATED", "AGGY_VENDOR", "CODEX_HOME"} {
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

var _ = store.ErrNotFound
