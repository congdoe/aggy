package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/tooldef"
)

// findingLines renders findings as aggy doctor prints them.
func findingLines(fs []checkup.Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString(f.Level.String() + " " + f.Code + " (" + string(f.Vendor) + "): " + f.Text("") + "\n")
	}
	return b.String()
}

// TestDoctorTools (T6710): doctor checks the tools against the switches
// per tool and vendor, read-only, one line per problem with its reason
// code and the command that fixes it.
func TestDoctorTools(t *testing.T) {
	ctx := context.Background()
	onPath := func(t *testing.T, progs ...string) {
		dir := t.TempDir()
		for _, p := range progs {
			writeAt(t, filepath.Join(dir, p), "#!/bin/sh\n", 0o755)
		}
		t.Setenv("PATH", dir)
	}
	for _, c := range []struct {
		name    string
		prepare func(t *testing.T, repo string)
		want    []string
		notWant []string
	}{
		{
			name: "nothing on",
			want: []string{"info tools-ok (): no managed tool is on"},
		},
		{
			name: "on and wireable; grok gated",
			prepare: func(t *testing.T, _ string) {
				onPath(t, "context7-mcp")
				toolOK(t, "on", "context7")
			},
			want:    []string{"info tool.vendor_unverified (grok): context7: on for grok, not wired: unverified"},
			notWant: []string{"warning"},
		},
		{
			name: "program missing",
			prepare: func(t *testing.T, _ string) {
				onPath(t)
				toolOK(t, "on", "context7", "--vendor", "codex")
			},
			want: []string{"warning tool.exec_refused (): context7: its program context7-mcp is not an executable on PATH, so it cannot start; fix: install context7 4.4.4, or aggy tool off context7"},
		},
		{
			name: "name clash",
			prepare: func(t *testing.T, _ string) {
				onPath(t, "context7-mcp")
				writeAt(t, filepath.Join(os.Getenv("HOME"), ".claude.json"), `{"mcpServers":{"context7":{"command":"x"}}}`, 0o600)
				toolOK(t, "on", "context7", "--vendor", "claude")
			},
			want: []string{"warning tool.name_clash (claude): context7: not wired into claude: a server named context7 is already set in", "fix: rename that server in the CLI's config, or aggy tool off context7 --vendor claude"},
		},
		{
			name: "user tool changed since add",
			prepare: func(t *testing.T, _ string) {
				traceTool(t, "tracer")
				writeAt(t, filepath.Join(os.Getenv(home.Env), "tools", "tracer.toml"), tracerTool+"# edited\n", 0o600)
			},
			want: []string{"warning tool.changed_since_add (): a user tool: its file is not the one aggy tool add showed, so it stays off: tool tracer; fix: aggy tool remove tracer"},
		},
		{
			name: "refused user definition",
			prepare: func(t *testing.T, _ string) {
				writeAt(t, filepath.Join(os.Getenv(home.Env), "tools", "bad.toml"), "name = \"bad\"\nbogus = 1\n", 0o600)
			},
			want: []string{"warning tool.def_invalid (): a user definition is refused, so its tool is off", "fix: the key it names, or aggy tool remove bad"},
		},
		{
			name: "skill links against the switches",
			prepare: func(t *testing.T, _ string) {
				// caveman on everywhere but grok, its claude link then
				// removed by hand and grok's ignore entry dropped.
				traceTool(t, "caveman")
				toolOK(t, "on", "caveman")
				toolOK(t, "off", "caveman", "--vendor", "grok")
				h := os.Getenv("HOME")
				if err := os.Remove(filepath.Join(h, ".claude", "skills", "caveman")); err != nil {
					t.Fatal(err)
				}
				writeAt(t, filepath.Join(h, ".grok", "config.toml"), "", 0o600)
				// A user tool's link, left after its file was deleted
				// by hand.
				src := filepath.Join(t.TempDir(), "tracer.toml")
				writeAt(t, src, tracerTool, 0o644)
				if code, _, errOut := toolRun("", true, "add", src, "--yes"); code != 0 {
					t.Fatal(errOut)
				}
				writeAt(t, filepath.Join(os.Getenv(home.Env), "skills", "tracer", "SKILL.md"), "---\nname: tracer\ndescription: t\n---\n", 0o644)
				toolOK(t, "on", "tracer", "--vendor", "codex")
				if err := os.Remove(filepath.Join(os.Getenv(home.Env), "tools", "tracer.toml")); err != nil {
					t.Fatal(err)
				}
			},
			want: []string{
				"warning tool-skill-unlinked (claude): caveman: on for claude, but its skill is not linked; fix: aggy tool on caveman --vendor claude",
				"warning tool-grok-ignore (grok): caveman: off for grok, but [skills] ignore lacks its skill, so grok loads it through the Claude or Codex link; fix: aggy tool off caveman --vendor grok",
				"warning tool-skill-stray (codex): a user tool: no tool of this name is loaded, but aggy's link to its skill stays, so codex loads it: tool tracer; fix: aggy tool off tracer",
			},
		},
		{
			name: "skill folder changed",
			prepare: func(t *testing.T, _ string) {
				traceTool(t, "caveman")
				toolOK(t, "on", "caveman", "--vendor", "claude")
				writeAt(t, filepath.Join(os.Getenv(home.Env), "skills", "caveman", "SKILL.md"), "---\nname: caveman\ndescription: changed\n---\n", 0o644)
			},
			want: []string{"warning tool.skill_changed (): caveman: its skill folder changed since it was first linked", "fix: aggy tool off caveman, read the folder, then aggy tool on caveman"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			wireHome(t)
			if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".grok"), 0o700); err != nil {
				t.Fatal(err)
			}
			repo := switchRepo(t, "")
			if c.prepare != nil {
				c.prepare(t, repo)
			}
			before := treeState(t)
			got := findingLines(toolFindings(ctx, repo))
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n%s", w, got)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(got, w) {
					t.Errorf("unexpected %q in:\n%s", w, got)
				}
			}
			if after := treeState(t); after != before {
				t.Errorf("doctor changed files:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

// treeState lists every path under HOME and the state dir with its
// size, mode, and link target: what a read-only doctor must leave as is.
func treeState(t *testing.T) string {
	t.Helper()
	var b strings.Builder
	for _, root := range []string{os.Getenv("HOME"), os.Getenv(home.Env)} {
		err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			dst, _ := os.Readlink(p)
			b.WriteString(p + " " + fi.Mode().String() + " " + dst)
			if fi.Mode().IsRegular() {
				data, _ := os.ReadFile(p)
				b.WriteString(" " + string(data))
			}
			b.WriteString("\n")
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return b.String()
}

// TestDoctorToolHookApproves (T6710): a hook that approves is reported
// as refused (tool.hook_approves), as a launch refuses it.
func TestDoctorToolHookApproves(t *testing.T) {
	wireHome(t)
	tool := tooldef.Tool{BuiltIn: true, Digest: strings.Repeat("ab", 32), Def: tooldef.Def{Name: "yes", Version: "1.0.0", Scope: "global",
		Hooks: []tooldef.Hook{{Vendors: []adapter.Vendor{adapter.Claude}, Event: "PreToolUse", Decides: true, Approves: true, Argv: []string{"yes", "hook"}}}}}
	b := switchboard.Board{Tools: []tooldef.Tool{tool}, Switches: []store.ToolSwitch{{Tool: "yes", On: true, Digest: switchboard.BuiltInDigest}}}
	got := findingLines(boardFindings(context.Background(), b, nil, "", toolRepoAt{}))
	if !strings.Contains(got, "warning tool.hook_approves (claude): yes: not wired into claude: its PreToolUse hook approves actions you would be asked about, so aggy never wires it; fix: aggy tool off yes --vendor claude") {
		t.Errorf("findings:\n%s", got)
	}
}
