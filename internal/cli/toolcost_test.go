package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/tooldef"
)

// TestToolCost (T6728): each tool's estimated tokens per vendor, offline:
// 0 when off or not wired (Grok's MCP servers and hooks), a static
// [cost] for the vendor or for all, a skill's SKILL.md as bytes / 4
// (frontmatter every turn, body on invoke), and unknown for an MCP
// server's schemas or a skill not in place.
func TestToolCost(t *testing.T) {
	wireHome(t)
	skill := "---\nname: caveman\ndescription: short replies\n---\n" + strings.Repeat("x", 400) // 49 bytes of frontmatter, 400 of body
	writeAt(t, filepath.Join(os.Getenv(home.Env), "skills", "caveman", "SKILL.md"), skill, 0o644)
	mcp := []tooldef.MCP{{Vendors: adapter.Vendors(), Argv: []string{"srv"}}}
	sk := []tooldef.Skill{{Vendors: adapter.Vendors(), Path: "~/.aggy/skills/caveman"}}
	tool := func(method string, tokens int, per map[string]int, m []tooldef.MCP, s []tooldef.Skill) tooldef.Tool {
		return tooldef.Tool{Def: tooldef.Def{Name: "caveman", MCP: m, Skills: s, Cost: tooldef.Cost{Method: method, Tokens: tokens, Vendor: per}}}
	}
	for _, c := range []struct {
		name         string
		t            tooldef.Tool
		v            adapter.Vendor
		skill, parts bool
		want         string
	}{
		{"off", tool("static", 400, nil, mcp, nil), adapter.Claude, false, false, "0"},
		{"static", tool("static", 400, nil, mcp, nil), adapter.Codex, true, true, "~400"},
		{"static per vendor", tool("static", 400, map[string]int{"codex": 650}, mcp, nil), adapter.Codex, true, true, "~650"},
		{"static, only another vendor's", tool("static", 0, map[string]int{"codex": 650}, mcp, nil), adapter.Claude, true, true, "unknown"},
		{"mcp schema", tool("mcp-schema", 0, nil, mcp, nil), adapter.Claude, true, true, "unknown"},
		{"mcp on grok, not wired", tool("mcp-schema", 0, nil, mcp, nil), adapter.Grok, true, true, "0"},
		{"none", tool("none", 0, nil, mcp, nil), adapter.Codex, true, true, "unknown"},
		{"skill", tool("skill-md", 0, nil, nil, sk), adapter.Grok, true, true, "~13 +100"},
		{"skill and mcp", tool("skill-md", 0, nil, mcp, sk), adapter.Claude, true, true, "~13 +100 +unknown"},
		{"skill and mcp on grok", tool("skill-md", 0, nil, mcp, sk), adapter.Grok, true, true, "~13 +100"},
		// M83-4: a skill follows the setting for every repo, servers the
		// repo's: tools_off keeps the skill, a per-repo on wires no skill.
		{"skill only, tools_off", tool("skill-md", 0, nil, mcp, sk), adapter.Claude, true, false, "~13 +100"},
		{"mcp only, per-repo on", tool("skill-md", 0, nil, mcp, sk), adapter.Claude, false, true, "unknown"},
	} {
		if got := toolCost(c.t, c.v, c.skill, c.parts); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if err := os.Remove(filepath.Join(os.Getenv(home.Env), "skills", "caveman", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if got := toolCost(tool("skill-md", 0, nil, nil, sk), adapter.Claude, true, true); got != "unknown" {
		t.Errorf("skill not in place: %q", got)
	}
}

// TestToolListCost (T6728): aggy tool list shows each tool's cost per
// vendor, 0 where it is off; aggy doctor shows it for each tool on.
func TestToolListCost(t *testing.T) {
	_, repo, _ := traceTool(t, "caveman")
	t.Chdir(repo)
	toolOK(t, "on", "caveman")
	toolOK(t, "off", "caveman", "--vendor", "codex")
	_, out, _ := toolRun("", false, "list")
	if got := statusLine(t, out, "caveman"); !strings.HasPrefix(got, "caveman built-in off ~10 +2 0 ~10 +2 built-in:caveman.toml") {
		t.Errorf("caveman's row: %q\n%s", got, out)
	}
	if got := statusLine(t, out, "context7"); !strings.HasPrefix(got, "context7 built-in off 0 0 0 ") {
		t.Errorf("context7's row: %q", got)
	}
	if !strings.Contains(out, "\nCost per vendor, here:") || !strings.Contains(out, "DEFAULT  CLAUDE  CODEX  GROK") {
		t.Errorf("list:\n%s", out)
	}
	got := findingLines(toolFindings(context.Background(), repo))
	if !strings.Contains(got, "info tool-cost (): caveman: estimated tokens every turn (+ more on invoke): claude ~10 +2, codex 0, grok ~10 +2") {
		t.Errorf("doctor:\n%s", got)
	}

	// M83-4: the repo's tools_off does not hide a linked skill, so its
	// cost stays.
	writeAt(t, filepath.Join(repo, ".aggy", "config.toml"), `tools_off = ["caveman"]`, 0o600)
	_, out, _ = toolRun("", false, "list")
	if got := statusLine(t, out, "caveman"); !strings.HasPrefix(got, "caveman built-in off ~10 +2 0 ~10 +2 ") {
		t.Errorf("caveman under tools_off: %q", got)
	}
}

// TestToolListCostSkillChanged (#84 MED): a tool whose linked skill
// changed costs only its skill, since a launch wires none of its
// servers or hooks.
func TestToolListCostSkillChanged(t *testing.T) {
	_, repo, _ := traceTool(t, "tracer")
	t.Chdir(repo)
	toolOK(t, "on", "tracer", "--vendor", "claude")
	_, out, _ := toolRun("", false, "list")
	if got := statusLine(t, out, "tracer"); !strings.HasPrefix(got, "tracer user off ~10 +2 +unknown 0 0 ") {
		t.Fatalf("tracer's row: %q", got)
	}
	// R80-3: a launch wires no server of a tool whose skill changed, so
	// only the linked skill costs.
	writeAt(t, filepath.Join(os.Getenv(home.Env), "skills", "tracer", "SKILL.md"), "---\nname: tracer\ndescription: test\n---\nBody.\n\n", 0o644)
	_, out, _ = toolRun("", false, "list")
	if got := statusLine(t, out, "tracer"); !strings.HasPrefix(got, "tracer user off ~10 +2 0 0 ") {
		t.Errorf("tracer's row after its skill changed: %q", got)
	}
}

// TestToolListCostPerRepo (M83-4): a tool turned on for one repo wires
// its server there, but its skill, linked from the setting for every
// repo, is not linked, so it costs nothing.
func TestToolListCostPerRepo(t *testing.T) {
	_, repo, _ := traceTool(t, "tracer")
	t.Chdir(repo)
	toolOK(t, "on", "tracer", "--vendor", "codex", "--repo", repo)
	_, out, _ := toolRun("", false, "list")
	if got := statusLine(t, out, "tracer"); !strings.HasPrefix(got, "tracer user off 0 unknown 0 ") {
		t.Errorf("tracer's row: %q\n%s", got, out)
	}
}

// TestToolListIndexUnreadable (L83-9): tool list still lists the tools
// when the index cannot be read, and says the costs assume nothing on.
func TestToolListIndexUnreadable(t *testing.T) {
	wireHome(t)
	writeAt(t, filepath.Join(os.Getenv(home.Env), "aggy.db"), "not a database", 0o600)
	code, out, errOut := toolRun("", false, "list")
	if code != 0 || !strings.Contains(out, "caveman") || !strings.Contains(errOut, "costs are shown as if no tool were turned on") {
		t.Errorf("list: %d\n%s\n%s", code, out, errOut)
	}
}
