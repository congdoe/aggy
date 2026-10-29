package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/tooldef"
)

// tracerTool is a user tool with every part a user tool may have: an
// MCP server and a skill for every vendor, and a Claude hook.
const tracerTool = `name = "tracer"
description = "A user tool with every part"
version = "1.0.0"
scope = "global"

[install]
check = ["tracer", "--version"]

[[mcp]]
vendors = ["claude", "codex", "grok"]
argv = ["tracer", "serve"]

[[skill]]
vendors = ["claude", "codex", "grok"]
path = "~/.aggy/skills/tracer"

[[hook]]
vendors = ["claude"]
event = "PostToolUse"
argv = ["tracer", "hook"]

[cost]
method = "skill-md"
`

// traceCaps are every launch flag a CLI can have, so nothing is held
// back for a missing one.
var traceCaps = []string{probe.CapConfigOverride, probe.CapSettings, probe.CapRules}

// toolTraces lists what of tool t reaches vendor v launched in repo:
// an MCP server in any of the CLIs' forms, a hook, a deny rule added for
// its rewrites, a launch warning naming it, a line in the shared memory
// or in a brief's missing-tools list from any other vendor, and a skill
// in a folder v scans (for Grok, through any link its [skills] ignore
// does not hide).
func toolTraces(t *testing.T, tool tooldef.Tool, v adapter.Vendor, repo string) []string {
	t.Helper()
	name := tool.Name
	var out []string
	req := adapter.InteractiveRequest{Deny: []string{"Bash(git push origin main)"}}
	cleanup, warns := addMemory(v, traceCaps, repo, filepath.Join(t.TempDir(), "mem"), &req)
	defer cleanup()
	warns = append(warns, addTools(context.Background(), v, traceCaps, repo, filepath.Join(t.TempDir(), "launch"), nil, &req)...)
	for _, s := range req.MCP {
		if s.Name == name {
			out = append(out, "a -c server")
		}
	}
	if req.MCPConfigFile != "" {
		if _, ok := readClaudeMCP(t, req.MCPConfigFile).MCPServers[name]; ok {
			out = append(out, "an --mcp-config server")
		}
	}
	if req.AgentFile != "" {
		if b, err := os.ReadFile(req.AgentFile); err != nil || strings.Contains(string(b), `{"`+name+`":`) {
			out = append(out, "an --agent server")
		}
	}
	for ev, gs := range req.Hooks {
		for _, g := range gs {
			for _, h := range g.Hooks {
				if strings.Contains(h.Command, " hook-tool --vendor "+string(v)+" "+name+" ") {
					out = append(out, "a "+ev+" hook")
				}
			}
		}
	}
	for _, h := range tool.Hooks {
		if h.RewritePrefix != "" && slices.ContainsFunc(req.Deny, func(d string) bool { return strings.HasPrefix(d, "Bash("+h.RewritePrefix) }) {
			out = append(out, "a deny rule for its rewrites")
		}
	}
	for _, w := range warns {
		if strings.Contains(w, "tool "+name+" ") {
			out = append(out, "a warning: "+w)
		}
	}
	mem := req.Memory
	if req.MemoryFile != "" {
		b, _ := os.ReadFile(req.MemoryFile)
		mem += string(b)
	}
	if !strings.Contains(mem, "Keep commits small.") {
		t.Errorf("%s got no shared memory, so the memory check proves nothing: %q", v, mem)
	}
	if strings.Contains(mem, name) {
		out = append(out, "a line in the memory")
	}
	for _, from := range adapter.Vendors() {
		if from == v {
			continue
		}
		for _, m := range missingTools(from, v, repo, os.Getenv) {
			if strings.Contains(m, name) {
				out = append(out, "a missing-tools line from "+string(from))
			}
		}
	}
	l, err := layoutFor(name)
	if err != nil {
		t.Fatal(err)
	}
	vis, err := l.visible(string(v))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range vis {
		out = append(out, "a skill through "+p)
	}
	return out
}

// traceExpected lists the trace kinds toolTraces must find of t on v
// while it is on, one per part (L83-8): a skill always (it is linked for
// every repo); an MCP server, a hook, and a rewrite's deny rule except on
// Grok, whose launch wiring is gated (W71-4). Each is a prefix of a
// toolTraces line.
func traceExpected(t tooldef.Tool, v adapter.Vendor) []string {
	p := t.PartsFor(v)
	var out []string
	if p.Skill != nil {
		out = append(out, "a skill through ")
	}
	if v == adapter.Grok {
		return out
	}
	if p.MCP != nil {
		out = append(out, map[adapter.Vendor]string{adapter.Claude: "an --mcp-config server", adapter.Codex: "a -c server"}[v])
	}
	for _, h := range p.Hooks {
		out = append(out, "a "+h.Event+" hook")
		if h.RewritePrefix != "" {
			out = append(out, "a deny rule for its rewrites")
		}
	}
	return out
}

// traceTool sets up wiring for one tool in a fresh home with a Grok
// home and a repo with shared memory: a user tool is added, and a
// skill's folder is put in place. It returns the loaded tool, the repo,
// and the --repo arguments its switches need.
func traceTool(t *testing.T, name string) (tooldef.Tool, string, []string) {
	t.Helper()
	wireHome(t)
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".grok"), 0o700); err != nil {
		t.Fatal(err)
	}
	repo := switchRepo(t, "")
	writeAt(t, filepath.Join(repo, ".aggy", "memory.md"), "Keep commits small.\n", 0o644)
	if name == "tracer" {
		src := filepath.Join(t.TempDir(), "tracer.toml")
		writeAt(t, src, tracerTool, 0o644)
		if code, _, errOut := toolRun("", true, "add", src, "--yes"); code != 0 {
			t.Fatalf("add: %q", errOut)
		}
	}
	r, refused, err := tooldef.LoadDefault()
	if err != nil || len(refused) > 0 {
		t.Fatal(err, refused)
	}
	tool, ok := r.Get(name)
	if !ok {
		t.Fatalf("no tool %s", name)
	}
	if hasSkill(tool) {
		writeAt(t, filepath.Join(os.Getenv(home.Env), "skills", name, "SKILL.md"), "---\nname: "+name+"\ndescription: test\n---\nBody.\n", 0o644)
	}
	if len(tool.Install.Prebuild) > 0 {
		fakeCodegraph(t, "0") // aggy tool on --repo indexes the repo (T6711)
	}
	var scope []string
	if tool.Scope == "repo" {
		scope = []string{"--repo", repo}
	}
	return tool, repo, scope
}

func toolOK(t *testing.T, args ...string) {
	t.Helper()
	if code, _, errOut := toolRun("", false, args...); code != 0 {
		t.Fatalf("aggy tool %q: %d %s", args, code, errOut)
	}
}

// TestToolOffLeavesNoTrace (T6727): for every built-in and a user tool
// with every part, a tool turned on everywhere and then off for one
// vendor leaves that vendor no MCP flag, file, or server, no hook, no
// deny rule of its own, no skill in any folder it scans, and no line in
// the memory or in a brief's missing-tools list; for Grok, the skill is
// on [skills] ignore while a Claude or Codex link is in a folder Grok
// scans. The same holds for every vendor once it is off everywhere.
// While it is on, the same check finds it, so the proof is not vacuous.
func TestToolOffLeavesNoTrace(t *testing.T) {
	r, err := tooldef.Builtins()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range r.List() {
		names = append(names, tool.Name)
	}
	names = append(names, "tracer")
	for i, name := range names {
		for _, v := range adapter.Vendors() {
			// Subtests are numbered: a tool's name in a temp path would
			// look like a trace.
			t.Run(fmt.Sprintf("%d-%s", i, v), func(t *testing.T) {
				tool, repo, scope := traceTool(t, name)
				toolOK(t, append([]string{"on", name}, scope...)...)
				got := toolTraces(t, tool, v, repo)
				for _, want := range traceExpected(tool, v) {
					if !slices.ContainsFunc(got, func(g string) bool { return strings.HasPrefix(g, want) }) {
						t.Fatalf("%s is on for %s, but the check finds no %q: %q", name, v, want, got)
					}
				}

				toolOK(t, append([]string{"off", name, "--vendor", string(v)}, scope...)...)
				if tr := toolTraces(t, tool, v, repo); len(tr) > 0 {
					t.Errorf("%s off for %s left: %q", name, v, tr)
				}
				if l, _ := layoutFor(name); v == adapter.Grok && hasSkill(tool) {
					claude, _ := l.linked(l.links["claude"])
					codex, _ := l.linked(l.links["codex"])
					b, _ := os.ReadFile(l.grokConfig)
					if ok, err := grokIgnores(b, l.store); (claude || codex) != ok || err != nil {
						t.Errorf("%s: claude %v codex %v linked, grok ignores it %v (%v)", name, claude, codex, ok, err)
					}
				}

				toolOK(t, append([]string{"off", name}, scope...)...)
				for _, w := range adapter.Vendors() {
					if tr := toolTraces(t, tool, w, repo); len(tr) > 0 {
						t.Errorf("%s off everywhere left %s: %q", name, w, tr)
					}
				}
			})
		}
	}
}

// narrowedTracer is tracerTool with its skill for Claude only.
func narrowedTracer() string {
	return strings.Replace(tracerTool, "vendors = [\"claude\", \"codex\", \"grok\"]\npath = ", "vendors = [\"claude\"]\npath = ", 1)
}

// TestToolNarrowedSkillDoctorFix (H84-1): once an edit narrows a tool's
// skill to Claude, doctor reports the Codex and Grok links stray, and
// the fix it names clears the one it is for.
func TestToolNarrowedSkillDoctorFix(t *testing.T) {
	_, repo, _ := traceTool(t, "tracer")
	toolOK(t, "on", "tracer")
	writeAt(t, filepath.Join(os.Getenv(home.Env), "tools", "tracer.toml"), narrowedTracer(), 0o600)
	stray := "warning tool-skill-stray (codex): a user tool: off for codex, but its skill is still linked, so codex loads it: tool tracer; fix: aggy tool off tracer --vendor codex"
	if got := findingLines(toolFindings(context.Background(), repo)); !strings.Contains(got, stray) {
		t.Fatalf("doctor:\n%s", got)
	}
	// The file changed, so the tool is off everywhere; the fix unlinks
	// every vendor, and the pin goes once no link is left.
	toolOK(t, "off", "tracer", "--vendor", "codex")
	got := findingLines(toolFindings(context.Background(), repo))
	if strings.Contains(got, "tool-skill-stray") || strings.Contains(got, "tool-grok-ignore") {
		t.Errorf("after the fix:\n%s", got)
	}
	l, _ := layoutFor("tracer")
	for _, v := range []string{"claude", "codex", "grok"} {
		if ok, _ := l.linked(l.links[v]); ok {
			t.Errorf("%s still linked", v)
		}
	}
}

// changeSkill edits tracer's installed skill after its first link.
func changeSkill(t *testing.T) {
	t.Helper()
	writeAt(t, filepath.Join(os.Getenv(home.Env), "skills", "tracer", "SKILL.md"), "---\nname: tracer\ndescription: changed\n---\n", 0o644)
}

// TestToolGoneLeavesNoSkill (T6727): a user tool whose skill is linked
// is off once its file is edited, removed, or refused; aggy tool remove,
// and aggy tool off of a name no longer loaded, take its skill links and
// Grok's ignore entry away, where they used to stay.
func TestToolGoneLeavesNoSkill(t *testing.T) {
	for _, c := range []struct {
		name string
		gone func(t *testing.T, file string)
	}{
		{"remove", func(t *testing.T, file string) {
			writeAt(t, file, tracerTool+"# edited\n", 0o600)
			toolOK(t, "remove", "tracer")
		}},
		// H84-1: off of a loaded tool whose edit narrowed its skill's
		// vendors unlinks the vendors it dropped too.
		{"edit narrows skill vendors, then off", func(t *testing.T, file string) {
			writeAt(t, file, narrowedTracer(), 0o600)
			toolOK(t, "off", "tracer")
		}},
		// H83-2: off of a loaded tool whose edit dropped its [[skill]].
		{"edit drops skill, then off", func(t *testing.T, file string) {
			writeAt(t, file, strings.Replace(strings.Replace(tracerTool, "[[skill]]\nvendors = [\"claude\", \"codex\", \"grok\"]\npath = \"~/.aggy/skills/tracer\"\n", "", 1), "skill-md", "none", 1), 0o600)
			toolOK(t, "off", "tracer")
		}},
		{"off when deleted", func(t *testing.T, file string) {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			toolOK(t, "off", "tracer")
		}},
		// R80-3: a skill changed since its first link still unlinks.
		{"off after the skill changed", func(t *testing.T, _ string) {
			changeSkill(t)
			toolOK(t, "off", "tracer")
		}},
		{"remove after the skill and file changed", func(t *testing.T, file string) {
			changeSkill(t)
			writeAt(t, file, tracerTool+"# edited\n", 0o600)
			toolOK(t, "remove", "tracer")
		}},
		{"off when refused", func(t *testing.T, file string) {
			writeAt(t, file, tracerTool+"bogus = 1\n", 0o600)
			toolOK(t, "off", "tracer")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tool, repo, _ := traceTool(t, "tracer")
			toolOK(t, "on", "tracer")
			toolOK(t, "off", "tracer", "--vendor", "grok")
			c.gone(t, filepath.Join(os.Getenv(home.Env), "tools", "tracer.toml"))
			for _, v := range adapter.Vendors() {
				if tr := toolTraces(t, tool, v, repo); len(tr) > 0 {
					t.Errorf("%s: %q", v, tr)
				}
			}
			l, _ := layoutFor("tracer")
			if b, _ := os.ReadFile(l.grokConfig); strings.Contains(string(b), l.store) {
				t.Errorf("grok's ignore entry stayed:\n%s", b)
			}
		})
	}
}

// TestToolOffForgetsSkillPin (L83-5): off with no link left to remove
// (the links went by hand) still forgets the skill's pin, so a changed
// folder can be turned on again after it is read.
func TestToolOffForgetsSkillPin(t *testing.T) {
	traceTool(t, "caveman")
	toolOK(t, "on", "caveman", "--vendor", "claude")
	h := os.Getenv("HOME")
	if err := os.Remove(filepath.Join(h, ".claude", "skills", "caveman")); err != nil {
		t.Fatal(err)
	}
	writeAt(t, filepath.Join(h, ".grok", "config.toml"), "", 0o600)
	writeAt(t, filepath.Join(os.Getenv(home.Env), "skills", "caveman", "SKILL.md"), "---\nname: caveman\ndescription: changed\n---\n", 0o644)
	if code, _, errOut := toolRun("", false, "on", "caveman", "--vendor", "claude"); code == 0 || !strings.Contains(errOut, "tool.skill_changed") {
		t.Fatalf("on with a changed folder: %d %q", code, errOut)
	}
	toolOK(t, "off", "caveman")
	toolOK(t, "on", "caveman", "--vendor", "claude")
}

// TestToolRemoveBuiltinNamed (L83-6): removing a refused user file named
// like a built-in leaves the built-in's settings and skill links alone.
func TestToolRemoveBuiltinNamed(t *testing.T) {
	traceTool(t, "caveman")
	toolOK(t, "on", "caveman", "--vendor", "claude")
	writeAt(t, filepath.Join(os.Getenv(home.Env), "tools", "caveman.toml"), "name = \"caveman\"\n", 0o600)
	code, out, errOut := toolRun("", false, "remove", "caveman")
	if code != 0 || !strings.Contains(out, "also a built-in tool; its settings and skill links are left as they are") {
		t.Fatalf("remove: %d %q %q", code, out, errOut)
	}
	l, _ := layoutFor("caveman")
	if ok, _ := l.linked(l.links["claude"]); !ok {
		t.Error("the built-in's claude link went")
	}
	if _, out, _ := toolRun("", false, "status", "caveman"); !strings.Contains(statusLine(t, out, "caveman"), "caveman on off off") {
		t.Errorf("the built-in's setting went:\n%s", out)
	}
}
