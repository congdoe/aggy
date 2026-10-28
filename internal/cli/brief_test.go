package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/brief"
	"github.com/congdoe/aggy/internal/transcript"
)

func TestBriefShow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	repo := t.TempDir()
	marker := gitRepo(t, repo)
	mustWrite(t, filepath.Join(repo, ".aggy", "task.md"), brief.TaskTemplate()+"- fix the parser\n", 0o600)
	mustWrite(t, filepath.Join(repo, ".aggy", "config.toml"), "allowed_vendors = [\"claude\", \"codex\"]\n", 0o600)
	id := "00000000-0000-4000-8000-000000000001"
	mustWrite(t, filepath.Join(home, ".claude", "projects", transcript.ClaudeProjectDir(repo), id+".jsonl"),
		`{"type":"user","message":{"content":"make the parser fast"}}`+"\n", 0o600)

	e := gitEnv()
	e.getenv = func(k string) string { return map[string]string{"HOME": home}[k] }
	e.cwd = func() (string, error) { return repo, nil }

	var out, errb bytes.Buffer
	if code := briefWith([]string{"--show", "--from", "claude", "--session", id, "--to", "codex"}, e, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	for _, want := range []string{"- fix the parser", "make the parser fast", "M a.go", "a.go |", id + ".jsonl"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("brief lacks %q:\n%s", want, out.String())
		}
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a program from the repo's git config ran")
	}

	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"--from", "claude", "--session", id}, 2},                           // no --show
		{[]string{"--show", "--from", "gpt", "--session", id}, 2},                    // bad vendor
		{[]string{"--show", "--from", "claude", "--session", "-x"}, 2},               // bad id
		{[]string{"--show", "--from", "claude", "--session", id, "--to", "grok"}, 1}, // not allowed
	} {
		out.Reset()
		errb.Reset()
		if code := briefWith(tc.args, e, &out, &errb); code != tc.code {
			t.Errorf("%v: exit %d, want %d (%s)", tc.args, code, tc.code, errb.String())
		}
	}

	// A missing transcript degrades to the rest of the brief.
	out.Reset()
	other := "00000000-0000-4000-8000-000000000002"
	if code := briefWith([]string{"--show", "--from", "grok", "--session", other}, e, &out, &errb); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "transcript could not be read") || !strings.Contains(out.String(), "- fix the parser") {
		t.Errorf("degraded brief:\n%s", out.String())
	}
}
