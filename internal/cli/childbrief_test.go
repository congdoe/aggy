package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/store"
)

// writeCodexTurns writes a Codex session file for id under the temp HOME
// with one user turn at each of times.
func writeCodexTurns(t *testing.T, id string, turns map[string]time.Time, order []string) {
	t.Helper()
	p := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "10", "28", "rollout-x-"+id+".jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, text := range order {
		fmt.Fprintf(&b, `{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}`+"\n",
			turns[text].UTC().Format(time.RFC3339Nano), text)
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

// T6671, T6674: a child coming back gets only the turns other CLIs added
// since it was last closed, and a new child the latest turns of the
// child before it; the brief is written under .aggy/briefs and can be
// previewed with aggy resume --brief --show.
func TestChildBrief(t *testing.T) {
	s := sessionStore(t)
	ctx := context.Background()
	dir := filepath.Join(os.Getenv("HOME"), "work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const pid = "cccc0000-0000-4000-8000-000000000000"
	seedParent(t, s, pid, dir,
		store.Child{ID: "c1", Vendor: "claude", VendorSessionID: sidA},
		store.Child{ID: "c2", Vendor: "codex", VendorSessionID: sidB})
	cs, err := s.Children(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	var claude store.Child
	for _, c := range cs {
		if c.ID == "c1" {
			claude = c
		}
	}
	closed := claude.LastActive
	if err := s.SetChildActivity(ctx, "c2", closed.Add(30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	writeCodexTurns(t, sidB, map[string]time.Time{
		"BEFORE close": closed.Add(-10 * time.Minute),
		"AFTER close":  closed.Add(10 * time.Minute),
	}, []string{"BEFORE close", "AFTER close"})
	parent, err := s.GetParent(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}

	env := defaultBorrowEnv()
	back, err := childBrief(ctx, s, env, launchPlan{parent: parent, child: claude})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(back, "# Return brief") || !strings.Contains(back, "AFTER close") || strings.Contains(back, "BEFORE close") {
		t.Errorf("return brief:\n%s", back)
	}

	fresh := store.Child{ID: "c3", ParentID: pid, Vendor: "grok", Cwd: dir}
	first, err := childBrief(ctx, s, env, launchPlan{parent: parent, child: fresh, newChild: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "# Handoff brief") || !strings.Contains(first, "running in codex") || !strings.Contains(first, "BEFORE close") {
		t.Errorf("first-visit brief:\n%s", first)
	}

	// A repo whose allowed_vendors leaves the child out gets no brief.
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".aggy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".aggy", "config.toml"), []byte("allowed_vendors = [\"codex\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := childBrief(ctx, s, env, launchPlan{parent: parent, child: claude}); err == nil || !strings.Contains(err.Error(), "allowed_vendors") {
		t.Errorf("disallowed vendor: err = %v", err)
	}
	if err := os.Remove(filepath.Join(dir, ".aggy", "config.toml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}

	// --brief --show previews and launches nothing; --show alone is a
	// usage error.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("AGGY_PARENT", "")
	var out, errb bytes.Buffer
	if code := Run([]string{"resume", "--show"}, &out, &errb); code != 2 {
		t.Errorf("resume --show = %d", code)
	}
	if code := Run([]string{"resume", "--brief", "--show", "cccc0000"}, &out, &errb); code != 0 {
		t.Fatalf("resume --brief --show = %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "brief") {
		t.Errorf("preview:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(dir, ".aggy", "briefs")); !os.IsNotExist(err) {
		t.Errorf("preview wrote a brief: %v", err)
	}
}
