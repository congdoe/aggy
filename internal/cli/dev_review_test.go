package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/devfix"
	"github.com/congdoe/aggy/internal/fakecli"
)

// Regression tests for the second PR #5 review. Each fails on bc0daa5.

// TestDevCaptureIgnoresStrayUUIDs (N1): a reply that mentions another
// session's id does not copy that session's transcript.
func TestDevCaptureIgnoresStrayUUIDs(t *testing.T) {
	fakes := fakecli.Build(t)
	setHome(t)
	root := fakeRepo(t)
	id := "0b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e"
	other := "7c4f1e2a-3b5d-4a6c-9e8f-1a2b3c4d5e6f"
	sc := fakecli.WriteScenario(t, `{"session_id":"`+id+`",
		"session_files":[
			{"path":"sessions/2026/10/27/rollout-x-{session_id}.jsonl","content":"mine\n"},
			{"path":"sessions/2026/10/27/rollout-x-`+other+`.jsonl","content":"someone else's session\n"}],
		"events":[{"type":"item.completed","item":{"type":"agent_message","text":"see also session `+other+`"}}]}`)
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dev", "capture", "codex", "--" + devConfirmFlag, "--name", "stray", "--bin", fakes.Codex,
		"--env", fakecli.EnvScenario + "=" + sc, "--", "exec", "--json", "hi"}, &stdout, &stderr); code != 0 {
		t.Fatalf("capture exit %d: %s", code, stderr.String())
	}
	b, err := os.ReadFile(filepath.Join(devfix.FixturesDir(root), "codex", "stray", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta devfix.Meta
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	if len(meta.Files) != 1 || !strings.HasSuffix(meta.Files[0], devfix.UUIDPrefix+"000000000001.jsonl") {
		t.Errorf("files = %v", meta.Files)
	}
	got, _ := os.ReadFile(filepath.Join(devfix.FixturesDir(root), "codex", "stray", "files", filepath.FromSlash(meta.Files[0])))
	if string(got) != "mine\n" {
		t.Errorf("copied %q", got)
	}
	if !slices.ContainsFunc(meta.Skipped, func(s string) bool { return strings.Contains(s, "not a file of this run's session") }) {
		t.Errorf("skipped = %v", meta.Skipped)
	}
}

// TestDevFixturesSymlinkRefused (N3): a symlinked testdata/fixtures does
// not move the fixtures-only limit elsewhere.
func TestDevFixturesSymlinkRefused(t *testing.T) {
	setHome(t)
	root := fakeRepo(t)
	elsewhere := t.TempDir()
	fx := devfix.FixturesDir(root)
	if err := os.Remove(fx); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, fx); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(elsewhere, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(elsewhere, "a", "stdout")
	if err := os.WriteFile(p, []byte("/Users/someone/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dev", "scrub", filepath.Join(fx, "a")}, &stdout, &stderr); code == 0 {
		t.Error("scrub through a symlinked testdata/fixtures succeeded")
	}
	if b, _ := os.ReadFile(p); string(b) != "/Users/someone/x\n" {
		t.Errorf("file rewritten: %q", b)
	}
	if code := Run([]string{"dev", "capture", "codex", "--" + devConfirmFlag, "--name", "x", "--bin", "/bin/echo", "--", "hi"},
		&stdout, &stderr); code == 0 {
		t.Error("capture through a symlinked testdata/fixtures succeeded")
	}
}
