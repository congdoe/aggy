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

// TestDevCaptureFake captures a fake Codex image run: it never runs a real
// CLI. The fixture holds the output, the images the run wrote, and no
// home path or session UUID.
func TestDevCaptureFake(t *testing.T) {
	fakes := fakecli.Build(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGGY_HOME", t.TempDir())
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		t.Setenv(v, "")
	}
	id := "0b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e"
	sc := fakecli.WriteScenario(t, `{"session_id":"`+id+`","images":2,
		"events":[{"type":"item.completed","item":{"type":"agent_message","text":"saved in {vendor_home}"}}]}`)
	out := filepath.Join(t.TempDir(), "fixture")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"dev", "capture", "codex", "--" + devConfirmFlag, "--out", out, "--bin", fakes.Codex,
		"--env", fakecli.EnvScenario + "=" + sc, "--", "exec", "--json", "draw a cat"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("capture exit %d: %s", code, stderr.String())
	}
	b, err := os.ReadFile(filepath.Join(out, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var meta devfix.Meta
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	placeholder := "00000000-0000-4000-8000-000000000001"
	wantFiles := []string{
		"vendor_home/generated_images/" + placeholder + "/1.png",
		"vendor_home/generated_images/" + placeholder + "/2.png",
	}
	if meta.Vendor != "codex" || meta.ExitCode != 0 || !slices.Equal(meta.Files, wantFiles) {
		t.Errorf("meta = %+v", meta)
	}
	for _, f := range wantFiles {
		if _, err := os.Stat(filepath.Join(out, "files", f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	so, _ := os.ReadFile(filepath.Join(out, "stdout"))
	if !strings.Contains(string(so), placeholder) || strings.Contains(string(so), id) || strings.Contains(string(so), home) ||
		!strings.Contains(string(so), "$HOME/.codex") {
		t.Errorf("stdout = %s", so)
	}
	findings, err := devfix.Detect(out, home, nil)
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
	t.Setenv("HOME", t.TempDir())
	out := filepath.Join(t.TempDir(), "fixture")
	for _, args := range [][]string{
		{"dev", "capture", "codex", "--out", out, "--bin", "/bin/echo", "--", "hi"},
		{"dev", "capture", "codex", "--" + devConfirmFlag, "--bin", "/bin/echo", "--", "hi"},
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
	if _, err := os.Stat(out); err == nil {
		t.Error("a refused capture wrote a fixture")
	}
	var stdout, stderr bytes.Buffer
	Run([]string{"dev", "capture", "codex", "--out", out, "--", "x"}, &stdout, &stderr)
	if !strings.Contains(stderr.String(), "--"+devConfirmFlag) {
		t.Errorf("refusal does not name the flag: %q", stderr.String())
	}
}

func TestDevScrub(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	p := filepath.Join(dir, "stdout")
	if err := os.WriteFile(p, []byte("at /Users/someone/x by me@corp.io\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"dev", "scrub", dir}, &stdout, &stderr); code != 0 {
		t.Fatalf("scrub exit %d: %s", code, stderr.String())
	}
	b, _ := os.ReadFile(p)
	if string(b) != "at $HOME/x by user@example.com\n" {
		t.Errorf("scrubbed = %q", b)
	}
	if code := Run([]string{"dev", "scrub"}, &stdout, &stderr); code != 2 {
		t.Errorf("scrub with no dir = %d", code)
	}
}
