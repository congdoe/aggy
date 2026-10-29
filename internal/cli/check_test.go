package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/tooldef"
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

// TestCheckRecordsNoErrorText (G67-R4): a check that cannot start is
// recorded with the code's text and the kind of failure, never the
// error's text, which names the repo's program and path.
func TestCheckRecordsNoErrorText(t *testing.T) {
	state := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", state)
	freshRun(t)
	root := checkRepo(t, `check = [["aggy-no-such-canary-tool"], ["./plain-canary.sh"]]`, map[string]string{"plain-canary.sh": "true\n"})
	e := defaultRepoCheckEnv()
	e.getwd = func() (string, error) { return root, nil }
	var out, errb bytes.Buffer
	if code := e.check(context.Background(), nil, &out, &errb); code != 1 || !strings.Contains(out.String(), "aggy-no-such-canary-tool") {
		t.Fatalf("code %d:\n%s%s", code, out.String(), errb.String())
	}
	text := allDiagText(t, state)
	if strings.Contains(text, "canary") || strings.Contains(text, root) {
		t.Errorf("a record holds the error's text:\n%s", text)
	}
	if strings.Count(text, `"failure":"start"`) != 2 || strings.Count(text, `"error_class":`) != 2 {
		t.Errorf("records lack the failure and its class:\n%s", text)
	}
}

// TestToolRefuseKey (F73-3): a [cost.vendor] key the user wrote reaches
// a record only when it names a vendor.
func TestToolRefuseKey(t *testing.T) {
	state := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", state)
	freshRun(t)
	var errb bytes.Buffer
	toolRefuse(context.Background(), &errb, "tool", diag.EvToolDefRefused,
		&tooldef.Error{File: "/x/t.toml", Key: "cost.vendor.marten-canary-81", Msg: "unknown vendor"})
	toolRefuse(context.Background(), &errb, "tool", diag.EvToolDefRefused,
		&tooldef.Error{File: "/x/t.toml", Key: "cost.vendor.grok", Msg: "want 1 to 9 tokens"})
	text := allDiagText(t, state)
	if strings.Contains(text, "canary") || !strings.Contains(text, "cost.vendor.<invalid>: refused") || !strings.Contains(text, `"key":"cost.vendor.\u003cinvalid\u003e"`) || !strings.Contains(text, "cost.vendor.grok") {
		t.Errorf("records:\n%s", text)
	}
	if !strings.Contains(errb.String(), "marten-canary-81") {
		t.Errorf("the printed error lost the key: %s", errb.String())
	}
}

func TestErrClass(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{&fs.PathError{Op: "open", Path: "/x", Err: fs.ErrNotExist}, "not_exist"},
		{fmt.Errorf("wrap: %w", fs.ErrPermission), "permission"},
		{fs.ErrExist, "exist"},
		{fmt.Errorf("x: %w", context.DeadlineExceeded), "timeout"},
		{context.Canceled, "canceled"},
		{&fs.PathError{Op: "open", Path: "/x", Err: errors.New("is a directory")}, "path"},
		{errors.New("/home/u/canary: no"), "other"},
	} {
		if got := errClass(c.err); got != c.want {
			t.Errorf("errClass(%v) = %q, want %q", c.err, got, c.want)
		}
	}
	rec := errRec(diag.Rec{Attrs: map[string]any{"kind": "k"}}, fs.ErrNotExist)
	if rec.Attrs["kind"] != "k" || rec.Attrs["error_class"] != "not_exist" {
		t.Errorf("errRec attrs %v", rec.Attrs)
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
