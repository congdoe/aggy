package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/parity"
)

// attribRepo makes an empty git repo, chdirs into it, and returns its
// root and a git helper that runs with extra env and returns stdout.
func attribRepo(t *testing.T) (string, func(env []string, args ...string) string) {
	t.Helper()
	repo, _ := filepath.EvalSymlinks(t.TempDir())
	git := func(env []string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = repo
		cmd.Env = append(append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "AGGY_VENDOR=", "AGGY_MODEL="), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git(nil, "init", "-q")
	t.Chdir(repo)
	return repo, git
}

// TestAttributionWire (T6692): the hook is installed through the
// manifest, adds the trailers only for valid AGGY_* values, and aggy
// unwire removes it.
func TestAttributionWire(t *testing.T) {
	isolate(t)
	repo, git := attribRepo(t)
	hook := filepath.Join(repo, ".git", "hooks", "commit-msg")
	_ = os.Remove(hook)
	out := mustRun(t, "attribution", "wire", "--dry-run")
	if !strings.Contains(out, "Aggy-Vendor") {
		t.Errorf("dry run shows no diff:\n%s", out)
	}
	if _, err := os.Stat(hook); !os.IsNotExist(err) {
		t.Fatal("the dry run wrote the hook")
	}
	mustRun(t, "attribution", "wire")
	fi, err := os.Stat(hook)
	if err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Fatalf("hook = %v, %v", fi, err)
	}
	if b, _ := os.ReadFile(hook); string(b) != parity.AttributionHook {
		t.Fatalf("hook text = %q", b)
	}
	if out := mustRun(t, "attribution", "wire"); !strings.Contains(out, "Nothing to change") {
		t.Errorf("rewire: %s", out)
	}
	commits := []struct {
		env  []string
		want string
	}{
		{[]string{"AGGY_VENDOR=codex", "AGGY_MODEL=gpt-5.1-codex"}, "Aggy-Vendor: codex\nAggy-Model: gpt-5.1-codex\n"},
		{[]string{"AGGY_VENDOR=grok"}, "Aggy-Vendor: grok\n"},
		{[]string{"AGGY_VENDOR=claude", "AGGY_MODEL=bad model$(touch x)"}, "Aggy-Vendor: claude\n"},
		{[]string{"AGGY_VENDOR=x;touch pwned", "AGGY_MODEL=m"}, ""},
		{[]string{"AGGY_VENDOR=" + strings.Repeat("a", 65)}, ""},
		{nil, ""},
	}
	for i, c := range commits {
		git(c.env, "commit", "-q", "--allow-empty", "-m", "change")
		got := git(nil, "log", "-1", "--format=%(trailers)")
		if strings.TrimSpace(got) != strings.TrimSpace(c.want) {
			t.Errorf("commit %d: trailers = %q, want %q", i, got, c.want)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, "pwned")); err == nil {
		t.Error("a trailer value ran a command")
	}
	mustRun(t, "unwire", "--all")
	if _, err := os.Stat(hook); !os.IsNotExist(err) {
		t.Errorf("unwire left the hook: %v", err)
	}
}

// TestAttributionWireRefuses (T6692): an existing hook is never
// replaced, and core.hooksPath makes wire refuse.
func TestAttributionWireRefuses(t *testing.T) {
	t.Run("existing hook", func(t *testing.T) {
		isolate(t)
		repo, _ := attribRepo(t)
		hook := filepath.Join(repo, ".git", "hooks", "commit-msg")
		writeAt(t, hook, "#!/bin/sh\nexit 0\n", 0o755)
		if code, _, errOut := run("attribution", "wire"); code != 1 || !strings.Contains(errOut, "never replaces a hook") {
			t.Errorf("exit %d: %s", code, errOut)
		}
		if b, _ := os.ReadFile(hook); string(b) != "#!/bin/sh\nexit 0\n" {
			t.Errorf("the person's hook changed: %q", b)
		}
	})
	t.Run("hooksPath", func(t *testing.T) {
		isolate(t)
		repo, git := attribRepo(t)
		git(nil, "config", "core.hooksPath", ".githooks")
		if code, _, errOut := run("attribution", "wire"); code != 1 || !strings.Contains(errOut, "core.hooksPath") {
			t.Errorf("exit %d: %s", code, errOut)
		}
		if _, err := os.Stat(filepath.Join(repo, ".git", "hooks", "commit-msg")); !os.IsNotExist(err) {
			t.Error("wire wrote a hook")
		}
	})
	t.Run("bad usage", func(t *testing.T) {
		for _, args := range [][]string{{"attribution"}, {"attribution", "wire", "x"}, {"attribution", "show"}} {
			if code, _, _ := run(args...); code != 2 {
				t.Errorf("%q: exit %d", args, code)
			}
		}
	})
}
