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

// fakeRepo makes a temp dir that looks like the aggy repo, with
// testdata/fixtures, and changes into it.
func fakeRepo(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+devfix.ModulePath+"\n\ngo 1.27\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(devfix.FixturesDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	return root
}

func setHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGGY_HOME", t.TempDir())
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		t.Setenv(v, "")
	}
	return home
}

// TestDevCaptureFake captures a fake Codex run: it never runs a real CLI.
// The fixture holds the output and this run's session file, and no
// image (binary), no other session's transcript, no home path, and no
// session UUID.
func TestDevCaptureFake(t *testing.T) {
	fakes := fakecli.Build(t)
	home := setHome(t)
	root := fakeRepo(t)
	id := "0b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e"
	other := "7c4f1e2a-3b5d-4a6c-9e8f-1a2b3c4d5e6f"
	sc := fakecli.WriteScenario(t, `{"session_id":"`+id+`","images":1,
		"session_files":[
			{"path":"sessions/2026/10/27/rollout-x-{session_id}.jsonl","content":"{\"home\":\"{home}\",\"id\":\"{session_id}\"}\n"},
			{"path":"sessions/2026/10/27/rollout-x-`+other+`.jsonl","content":"someone else's session\n"}],
		"events":[{"type":"item.completed","item":{"type":"agent_message","text":"saved in {vendor_home}"}}]}`)
	var stdout, stderr bytes.Buffer
	code := Run([]string{"dev", "capture", "codex", "--" + devConfirmFlag, "--name", "image-one", "--bin", fakes.Codex,
		"--env", fakecli.EnvScenario + "=" + sc, "--", "exec", "--json", "draw a cat"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("capture exit %d: %s", code, stderr.String())
	}
	out := filepath.Join(devfix.FixturesDir(root), "codex", "image-one")
	b, err := os.ReadFile(filepath.Join(out, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta devfix.Meta
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	placeholder := devfix.UUIDPrefix + "000000000001"
	want := []string{"vendor_home/sessions/2026/10/27/rollout-x-" + placeholder + ".jsonl"}
	if meta.ExitCode != 0 || !slices.Equal(meta.Files, want) {
		t.Errorf("files = %v, skipped %v", meta.Files, meta.Skipped)
	}
	skipped := strings.Join(meta.Skipped, "\n")
	if !strings.Contains(skipped, "1.png: binary") || !strings.Contains(skipped, "not a file of this run's session") {
		t.Errorf("skipped = %v", meta.Skipped)
	}
	so, _ := os.ReadFile(filepath.Join(out, "stdout"))
	if !strings.Contains(string(so), placeholder) || strings.Contains(string(so), id) || strings.Contains(string(so), home) {
		t.Errorf("stdout = %s", so)
	}
	findings, err := devfix.Detect(out, devfix.Identity{Home: home}, nil)
	if err != nil || len(findings) != 0 {
		t.Errorf("findings = %v, %v", findings, err)
	}
	// The fixture is never overwritten.
	if code := Run([]string{"dev", "capture", "codex", "--" + devConfirmFlag, "--out", out, "--bin", fakes.Codex, "--", "--version"},
		&stdout, &stderr); code == 0 {
		t.Error("capture overwrote an existing fixture")
	}
}

func TestDevCaptureRefusals(t *testing.T) {
	setHome(t)
	root := fakeRepo(t)
	out := filepath.Join(devfix.FixturesDir(root), "codex", "x")
	outside := filepath.Join(t.TempDir(), "fixture")
	for _, args := range [][]string{
		{"dev", "capture", "codex", "--out", out, "--bin", "/bin/echo", "--", "hi"},
		{"dev", "capture", "codex", "--" + devConfirmFlag, "--bin", "/bin/echo", "--", "hi"},
		{"dev", "capture", "codex", "--" + devConfirmFlag, "--out", outside, "--bin", "/bin/echo", "--", "hi"},
		{"dev", "capture", "codex", "--" + devConfirmFlag, "--out", filepath.Join(devfix.FixturesDir(root), "..", "x"), "--bin", "/bin/echo"},
		{"dev", "capture", "gemini", "--" + devConfirmFlag, "--out", out},
		{"dev", "capture", "codex", "--" + devConfirmFlag, "--name", "../x", "--bin", "/bin/echo"},
		{"dev", "capture", "codex", "--" + devConfirmFlag, "--out", out, "stray"},
		{"dev", "capture"},
		{"dev", "bogus"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(args, &stdout, &stderr); code == 0 {
			t.Errorf("%v: exit 0", args)
		}
	}
	for _, p := range []string{out, outside} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("a refused capture wrote %s", p)
		}
	}
	var stdout, stderr bytes.Buffer
	Run([]string{"dev", "capture", "codex", "--out", out, "--", "x"}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "--"+devConfirmFlag) {
		t.Errorf("refusal does not name the flag: %q", stderr.String())
	}
}

// TestDevScrub (10): scrub works under testdata/fixtures, and elsewhere
// only with --anywhere.
func TestDevScrub(t *testing.T) {
	setHome(t)
	root := fakeRepo(t)
	for _, tt := range []struct {
		dir      string
		args     []string
		wantCode int
	}{
		{filepath.Join(devfix.FixturesDir(root), "codex", "a"), nil, 0},
		{t.TempDir(), nil, 2},
		{t.TempDir(), []string{"--anywhere"}, 0},
	} {
		if err := os.MkdirAll(tt.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(tt.dir, "stdout")
		orig := "at /Users/someone/x by me@corp.io\n"
		if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		args := append(append([]string{"dev", "scrub"}, tt.args...), tt.dir)
		if code := Run(args, &stdout, &stderr); code != tt.wantCode {
			t.Errorf("%v: exit %d, want %d: %s", args, code, tt.wantCode, stderr.String())
		}
		b, _ := os.ReadFile(p)
		want := "at $HOME/x by user@example.com\n"
		if tt.wantCode != 0 {
			want = orig
		}
		if string(b) != want {
			t.Errorf("%v: file = %q, want %q", args, b, want)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dev", "scrub"}, &stdout, &stderr); code != 2 {
		t.Errorf("scrub with no dir = %d", code)
	}
}
