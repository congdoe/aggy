package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/claudetrust"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/trustlist"
)

// oldTrustRecord trusts dir's repo for aggy's rules as aggy trust did before
// the list: a repos entry, and no answer for the folder.
func oldTrustRecord(t *testing.T, dir string) {
	t.Helper()
	path, err := trustFile()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"repos": []string{mustKey(t, dir)}})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTrustInSessionWaits (PR #130 review, lost update): /aggy trust inside a
// running session records aggy's answer at once, and leaves Claude's file
// alone while that Claude may be writing it; the next start here writes
// Claude's key before the CLI starts, with the question turned off too, and
// a file changed underneath then is refused and tried again at the start
// after.
func TestTrustInSessionWaits(t *testing.T) {
	h, repo := trustWorld(t, "claude")
	path := filepath.Join(h, ".claude.json")
	if err := os.WriteFile(path, []byte(claudeJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	out := sessionTrustAnswer(repo)([]string{trustRowClaude}, trustlist.Confirm)
	if !strings.Contains(out, "written at the next aggy start here") || !strings.Contains(out, "Saved this folder's answer") {
		t.Fatalf("the page: %q", out)
	}
	if b, _ := os.ReadFile(path); string(b) != claudeJSON {
		t.Fatalf("the session's answer wrote Claude's file: %s", b)
	}
	if a, _ := folderAnswer(t, repo); !slices.Equal(a.Later, []string{trustRowClaude}) || !slices.Equal(a.On, []string{trustRowClaude}) {
		t.Fatalf("answer %+v", a)
	}
	// The answer counts at once for aggy's own rules (#129's pre-approvals
	// read them at the launch, after the trust list).
	if !loadPermissions(context.Background(), repo, config.Default()).Trusted {
		t.Error("the repo is not trusted for aggy's rules after the answer")
	}

	t.Run("changed underneath at the start", func(t *testing.T) {
		const meanwhile = `{"claude": "wrote this meanwhile"}`
		testHookTrustLaterPlanned = func() { _ = os.WriteFile(path, []byte(meanwhile), 0o600) }
		defer func() { testHookTrustLaterPlanned = nil }()
		out, note := startIn(repo, config.Default(), "")
		if !strings.Contains(out, "not written") || !strings.Contains(out, "tries again at the next start") || note != "" {
			t.Fatalf("%q %q", out, note)
		}
		if b, _ := os.ReadFile(path); string(b) != meanwhile {
			t.Fatalf("Claude's newer file was overwritten: %s", b)
		}
		if a, _ := folderAnswer(t, repo); !slices.Equal(a.Later, []string{trustRowClaude}) {
			t.Fatalf("the waiting change was dropped: %+v", a)
		}
	})

	t.Setenv(trustAskEnv, "0")
	out, note := startIn(repo, config.Default(), "")
	if !strings.Contains(out, "Wrote what your answer in a session left") || strings.Contains(out, "Pick what may run") || note != "" {
		t.Fatalf("the next start: %q %q", out, note)
	}
	b, _ := os.ReadFile(path)
	if st, _ := claudetrust.State(b, repo); st != claudetrust.WasTrue || !strings.Contains(string(b), "wrote this meanwhile") {
		t.Fatalf("the key was not written at the start: %s", b)
	}
	if a, _ := folderAnswer(t, repo); len(a.Later) != 0 {
		t.Errorf("still waiting: %+v", a)
	}
	if out, _ := startIn(repo, config.Default(), ""); out != "" {
		t.Errorf("the start after wrote again: %q", out)
	}
}

// TestTrustOldRecord (PR #130 review): in a repo aggy trust trusted before the
// list, the first list shows Claude Code new and unticked, saying ticking it
// also records Claude's own trust, so one Enter grants no more than before;
// aggy's own trust stays as it was whatever is ticked, and a later answer
// with no harness ticked takes it back as before.
func TestTrustOldRecord(t *testing.T) {
	h, repo := trustWorld(t, "claude", "opencode")
	path := filepath.Join(h, ".claude.json")
	if err := os.WriteFile(path, []byte(claudeJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTrustRecord(t, repo)
	out, _ := startIn(repo, config.Default(), "\n")
	if !strings.Contains(out, " 2 [ ] Claude Code   new") || !strings.Contains(out, "also records Claude's own trust for this folder") || !strings.Contains(out, "stays trusted for aggy's rules") {
		t.Fatalf("the list: %q", out)
	}
	if b, _ := os.ReadFile(path); string(b) != claudeJSON {
		t.Fatalf("one Enter wrote Claude's own trust: %s", b)
	}
	if !repoTrusted(mustKey(t, repo)) {
		t.Fatal("the first answer took aggy's old trust back")
	}
	// Only Claude's sandbox, ticked by default (T6980), which confines.
	if a, ok := folderAnswer(t, repo); !ok || !slices.Equal(a.On, []string{trustRowClaudeSandbox}) {
		t.Errorf("answer %+v", a)
	}
	if out, _ := startIn(repo, config.Default(), "\n"); out != "" {
		t.Errorf("asked again: %q", out)
	}
	atKeyboard(t, "\n")
	if code, out, errb := run("trust", repo); code != 0 || repoTrusted(mustKey(t, repo)) {
		t.Errorf("a later answer with nothing ticked: %d %q %q", code, out, errb)
	}
}

// TestTrustNoOldRecord: a folder aggy never trusted shows Claude Code
// unticked with no such line, and nothing ticked trusts nothing.
func TestTrustNoOldRecord(t *testing.T) {
	_, repo := trustWorld(t, "claude")
	out, _ := startIn(repo, config.Default(), "\n")
	if !strings.Contains(out, " 2 [ ] Claude Code") || strings.Contains(out, "also records") || repoTrusted(mustKey(t, repo)) {
		t.Errorf("%q", out)
	}
}
