package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// checkTools puts links to the named system tools on a temp dir at the
// front of PATH: the isolated test PATH holds only git, sh, cat, and
// touch, and the check commands these tests run need a few more.
func checkTools(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		found := false
		for _, d := range []string{"/bin", "/usr/bin"} {
			if fi, err := os.Stat(filepath.Join(d, n)); err == nil && fi.Mode().IsRegular() {
				if err := os.Symlink(filepath.Join(d, n), filepath.Join(dir, n)); err != nil {
					t.Fatal(err)
				}
				found = true
				break
			}
		}
		if !found {
			t.Skipf("no %s in /bin or /usr/bin", n)
		}
	}
	t.Setenv("PATH", dir+string(filepath.ListSeparator)+os.Getenv("PATH"))
}

func TestCheck(t *testing.T) {
	checkTools(t, "true", "sleep", "yes", "head", "env")
	scripts := map[string]string{
		"ok.sh":    "echo fine\n",
		"fail.sh":  "echo first\necho 'token ghp_abcdefghijklmnopqrstuvwxyz0123456789'\nenv\necho last >&2\nexit 3\n",
		"slow.sh":  "sleep 5\n",
		"plain.sh": "echo no exec bit\n",
		"long.sh":  "echo FIRST\nyes filler | head -c 3000000\necho\necho FINAL\nexit 1\n",
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
		{name: "long output shows its end", cfg: `check = [["./long.sh"]]`, code: 1,
			want: []string{"| filler\n", "| FINAL\n"}, dontWant: []string{"FIRST"}},
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
				e.runner = func(time.Duration) adapter.Runner { return execrun.CheckRunner(tt.timeout) }
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

// TestCheckInterrupt (H-1): Ctrl-C ends aggy check and kills the running
// check's process group.
func TestCheckInterrupt(t *testing.T) {
	checkTools(t, "sleep")
	pidFile := filepath.Join(t.TempDir(), "pid")
	root := checkRepo(t, `check = [["./wait.sh", "`+pidFile+`"]]`, map[string]string{
		"wait.sh": "echo $$ > \"$1\"\nexec sleep 777\n",
	})
	t.Chdir(root)
	go func() {
		for i := 0; i < 200; i++ {
			if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
				_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
	var out, errb bytes.Buffer
	start := time.Now()
	if code := runCheck(nil, &out, &errb); code != 1 || !strings.Contains(out.String(), "interrupted") {
		t.Fatalf("code %d\n%s%s", code, out.String(), errb.String())
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("interrupt took %s", d)
	}
	b, _ := os.ReadFile(pidFile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the check outlived aggy check")
		}
	}
}
