package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
)

// checkRepo makes a git-looking repo with the given repo config and
// executable scripts, and returns its root.
func checkRepo(t *testing.T, cfg string, scripts map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{".git", ".aggy"} {
		if err := safefs.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := safefs.WriteFile(filepath.Join(root, ".aggy", "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, body := range scripts {
		mode := os.FileMode(0o755)
		if strings.HasPrefix(name, "plain") {
			mode = 0o644
		}
		if err := safefs.WriteFile(filepath.Join(root, name), []byte("#!/bin/sh\n"+body), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestCheck(t *testing.T) {
	scripts := map[string]string{
		"ok.sh":    "echo fine\n",
		"fail.sh":  "echo first\necho 'token ghp_abcdefghijklmnopqrstuvwxyz0123456789'\nenv\necho last >&2\nexit 3\n",
		"slow.sh":  "sleep 5\n",
		"plain.sh": "echo no exec bit\n",
	}
	tests := []struct {
		name     string
		cfg      string
		timeout  time.Duration
		code     int
		want     []string
		dontWant []string
	}{
		{name: "pass", cfg: `check = [["./ok.sh"], ["true"]]`, code: 0,
			want: []string{"  ./ok.sh\n  true\n", "ok   ./ok.sh", "ok   true", "all 2 passed"}, dontWant: []string{"fine"}},
		{name: "fail shows redacted tail without keys", cfg: `check = [["./fail.sh"], ["./ok.sh"]]`, code: 1,
			want:     []string{"FAIL ./fail.sh", "exit status 3", "| first", "| last", "ok   ./ok.sh", "1 of 2 failed"},
			dontWant: []string{"ghp_", "OPENAI_API_KEY", "AGGY_TEST_SECRET"}},
		{name: "timeout", cfg: `check = [["./slow.sh"]]`, timeout: 300 * time.Millisecond, code: 1, want: []string{"FAIL ./slow.sh", "timed out"}},
		{name: "missing program", cfg: `check = [["aggy-no-such-tool"]]`, code: 1, want: []string{"cannot start"}},
		{name: "not executable", cfg: `check = [["./plain.sh"]]`, code: 1, want: []string{"not an executable file"}},
		{name: "none configured", cfg: ``, code: 1},
		{name: "bad config", cfg: `check = [["/bin/sh", "-c", "x"]]`, code: 2},
	}
	t.Setenv("OPENAI_API_KEY", "sk-should-not-pass")
	t.Setenv("AGGY_TEST_SECRET", "x")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := checkRepo(t, tt.cfg, scripts)
			e := defaultRepoCheckEnv()
			e.getwd = func() (string, error) { return root, nil }
			if tt.timeout > 0 {
				e.runner = func(time.Duration) adapter.Runner { return execrun.ExecRunner(tt.timeout) }
			}
			var out, errb bytes.Buffer
			if code := e.check(context.Background(), nil, &out, &errb); code != tt.code {
				t.Fatalf("code = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tt.code, out.String(), errb.String())
			}
			for _, w := range tt.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("stdout lacks %q:\n%s", w, out.String())
				}
			}
			for _, w := range tt.dontWant {
				if strings.Contains(out.String(), w) {
					t.Errorf("stdout holds %q:\n%s", w, out.String())
				}
			}
		})
	}
}

func TestCheckProgramStaysInRepo(t *testing.T) {
	root := checkRepo(t, "", nil)
	outside := t.TempDir()
	if err := safefs.WriteFile(filepath.Join(outside, "x.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "out")); err != nil {
		t.Fatal(err)
	}
	e := repoCheckEnv{lookPath: probe.LookPath}
	if _, err := e.program(root, "./out/x.sh"); err == nil || !strings.Contains(err.Error(), "outside the repo") {
		t.Fatalf("err = %v, want outside the repo", err)
	}
}
