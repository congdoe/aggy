package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/home"
)

// TestToolSkill: a tool's skill is linked from ~/.aggy/skills/<name>
// only when that folder is in place and checks out; aggy never fetches
// it. Off for Grok while Claude and Codex keep it puts the folder in
// Grok's [skills] ignore (T6709); off everywhere unlinks it all.
func TestToolSkill(t *testing.T) {
	h := isolate(t)
	if err := os.MkdirAll(filepath.Join(h, ".grok"), 0o700); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(os.Getenv(home.Env), "skills", "caveman")
	links := map[string]string{
		"claude": filepath.Join(h, ".claude", "skills", "caveman"),
		"codex":  filepath.Join(h, ".agents", "skills", "caveman"),
		"grok":   filepath.Join(h, ".grok", "skills", "caveman"),
	}
	linked := func() string {
		var out []string
		for _, v := range []string{"claude", "codex", "grok"} {
			if dst, err := os.Readlink(links[v]); err == nil && dst == store {
				out = append(out, v)
			}
		}
		return strings.Join(out, " ")
	}

	// Missing: refused, says how, links nothing, records nothing.
	code, _, errOut := toolRun("", false, "on", "caveman")
	if code != 1 || !strings.Contains(errOut, "tool.skill_missing") || !strings.Contains(errOut, "never fetches") || linked() != "" {
		t.Fatalf("missing: %d %q %q", code, errOut, linked())
	}
	if _, out, _ := toolRun("", false, "status", "caveman"); !strings.Contains(out, "off by default") {
		t.Errorf("a refused on was recorded:\n%s", out)
	}
	// A symlink inside the folder, or the folder itself a symlink.
	secret := filepath.Join(t.TempDir(), "secret")
	writeAt(t, secret, "x", 0o600)
	writeAt(t, filepath.Join(store, "SKILL.md"), "---\nname: caveman\ndescription: short\n---\n", 0o644)
	if err := os.Symlink(secret, filepath.Join(store, "ref.md")); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := toolRun("", false, "on", "caveman"); code != 1 || !strings.Contains(errOut, "tool.skill_missing") || linked() != "" {
		t.Errorf("a symlink in the folder: %d %q", code, errOut)
	}
	if err := os.Remove(filepath.Join(store, "ref.md")); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Rename(store, real); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, store); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := toolRun("", false, "on", "caveman"); code != 1 || !strings.Contains(errOut, "tool.skill_missing") || linked() != "" {
		t.Errorf("a symlinked folder: %d %q", code, errOut)
	}
	if err := os.Remove(store); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(real, store); err != nil {
		t.Fatal(err)
	}

	if code, _, errOut := toolRun("", false, "on", "caveman"); code != 0 || linked() != "claude codex grok" {
		t.Fatalf("on: %d %q %q", code, errOut, linked())
	}
	grokConfig := filepath.Join(h, ".grok", "config.toml")
	if code, _, errOut := toolRun("", false, "off", "caveman", "--vendor", "grok"); code != 0 || linked() != "claude codex" {
		t.Fatalf("off for grok: %d %q %q", code, errOut, linked())
	}
	if b, _ := os.ReadFile(grokConfig); !strings.Contains(string(b), store) {
		t.Errorf("grok's [skills] ignore lacks the skill:\n%s", b)
	}
	if code, _, errOut := toolRun("", false, "off", "caveman"); code != 0 || linked() != "" {
		t.Fatalf("off: %d %q %q", code, errOut, linked())
	}
	if b, _ := os.ReadFile(grokConfig); strings.Contains(string(b), store) {
		t.Errorf("the ignore entry stayed with no link left:\n%s", b)
	}
}
