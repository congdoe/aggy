package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
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

	// L78-8: a skill is never linked per repo, so --repo is refused.
	if code, _, errOut := toolRun("", false, "on", "caveman", "--repo", switchRepo(t, "")); code == 0 || !strings.Contains(errOut, "tool.not_applicable") || !strings.Contains(errOut, "only a skill") {
		t.Errorf("--repo for a skill: %d %q", code, errOut)
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
	// L78-4: a folder changed after it was linked is not linked again;
	// off everywhere forgets it, and the next on takes it as it is.
	writeAt(t, filepath.Join(store, "SKILL.md"), "---\nname: caveman\ndescription: changed\n---\n", 0o644)
	if code, _, errOut := toolRun("", false, "on", "caveman"); code != 1 || !strings.Contains(errOut, "tool.skill_changed") || linked() != "claude codex" {
		t.Fatalf("a changed skill: %d %q %q", code, errOut, linked())
	}
	if code, _, errOut := toolRun("", false, "off", "caveman"); code != 0 || linked() != "" {
		t.Fatalf("off: %d %q %q", code, errOut, linked())
	}
	if code, _, errOut := toolRun("", false, "on", "caveman", "--vendor", "claude"); code != 0 || linked() != "claude" {
		t.Fatalf("on again: %d %q %q", code, errOut, linked())
	}
	if code, _, errOut := toolRun("", false, "off", "caveman"); code != 0 || linked() != "" {
		t.Fatalf("off: %d %q %q", code, errOut, linked())
	}
	if b, _ := os.ReadFile(grokConfig); strings.Contains(string(b), store) {
		t.Errorf("the ignore entry stayed with no link left:\n%s", b)
	}
}

// TestAddToolsSkillChanged (R80-3): a launch checks the skill pin too. A
// tool that is on, whose linked skill folder changed since it was first
// linked, gets no MCP server for the launch, with tool.skill_changed;
// its link stays.
func TestAddToolsSkillChanged(t *testing.T) {
	src, _ := wireHome(t)
	writeAt(t, src, demoTool+"\n[[skill]]\nvendors = [\"claude\"]\npath = \"~/.aggy/skills/demo\"\n", 0o600)
	if code, _, errOut := toolRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatalf("add: %d %q", code, errOut)
	}
	skill := filepath.Join(os.Getenv(home.Env), "skills", "demo", "SKILL.md")
	writeAt(t, skill, "---\nname: demo\ndescription: short\n---\n", 0o644)
	if code, _, errOut := toolRun("", false, "on", "demo", "--vendor", "claude"); code != 0 {
		t.Fatalf("on: %d %q", code, errOut)
	}
	repo := switchRepo(t, "")
	req, warns, _ := wireOnce(t, adapter.Claude, nil, repo)
	if req.MCPConfigFile == "" || len(warns) != 0 {
		t.Fatalf("before the change: %+v %q", req, warns)
	}
	writeAt(t, skill, "---\nname: demo\ndescription: changed\n---\n", 0o644)
	req, warns, _ = wireOnce(t, adapter.Claude, nil, repo)
	if req.MCPConfigFile != "" || len(warns) != 1 || !strings.Contains(warns[0], "tool.skill_changed") {
		t.Errorf("a changed skill was wired: %+v %q", req, warns)
	}
	if _, err := os.Lstat(filepath.Join(os.Getenv("HOME"), ".claude", "skills", "demo")); err != nil {
		t.Errorf("the link went: %v", err)
	}
}
