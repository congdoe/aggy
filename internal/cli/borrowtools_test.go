package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/probe"
)

// TestEditTools (T6709): only an edit gets tools, its default is
// routing.edit_tools, and --tool may pick only names listed there.
func TestEditTools(t *testing.T) {
	r := config.Routing{EditTools: []string{"context7", "codegraph"}}
	tests := []struct {
		name  string
		kind  adapter.Kind
		asked []string
		want  []string
		code  diag.Code
	}{
		{"edit default", adapter.Edit, nil, []string{"context7", "codegraph"}, diag.Code{}},
		{"edit picks", adapter.Edit, []string{"codegraph"}, []string{"codegraph"}, diag.Code{}},
		{"edit unlisted", adapter.Edit, []string{"demo"}, nil, diag.CodeToolUnlisted},
		{"review gets none", adapter.Review, nil, nil, diag.Code{}},
		{"review refuses --tool", adapter.Review, []string{"context7"}, nil, diag.CodeToolBorrowKind},
		{"research refuses --tool", adapter.Research, []string{"context7"}, nil, diag.CodeToolBorrowKind},
		{"image refuses --tool", adapter.Image, []string{"context7"}, nil, diag.CodeToolBorrowKind},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, code, err := editTools(tt.kind, tt.asked, r)
			if code != tt.code || (err != nil) != (tt.code != diag.Code{}) || !slices.Equal(got, tt.want) {
				t.Errorf("got %q %v %v, want %q %v", got, code, err, tt.want, tt.code)
			}
		})
	}
}

// toolsFixture is a borrow fixture whose aggy path is testBin, with
// routing.edit_tools set.
func toolsFixture(t *testing.T, editTools string) *borrowFixture {
	t.Helper()
	f := newBorrowFixture(t, adapter.Codex, adapter.Grok, adapter.Claude)
	oldBin, oldPath, oldCheck := hookBin, pathAggy, checkTrusted
	hookBin = func() (string, error) { return testBin, nil }
	pathAggy = func() (string, error) { return "", os.ErrNotExist }
	checkTrusted = func(string) error { return nil }
	t.Cleanup(func() { hookBin, pathAggy, checkTrusted = oldBin, oldPath, oldCheck })
	mustWrite(t, filepath.Join(f.state, config.FileName), "[routing]\nedit_tools = "+editTools+"\n", 0o600)
	return f
}

// TestBorrowEditToolsCommand (T6709): an edit child on Codex gets the
// tools routing.edit_tools lists, each through aggy tool exec in a
// private launch folder removed once it exits, with its own approval;
// --tool picks among them; other kinds refuse --tool. A tool that
// reaches the network needs routing.edit_network_tools (M78-2).
func TestBorrowEditToolsCommand(t *testing.T) {
	f := toolsFixture(t, `["context7"]`)
	editRepo(t, f.repo)
	f.env.gitPath, f.env.gitRun = gitEnv().gitPath, gitEnv().gitRun
	f.scenario["codex"] = `{"events":[{"type":"item.completed","item":{"type":"agent_message","text":"done"}}]}`
	if code, _, errOut := f.run(t, "edit", "add a helper", "--vendor", "codex"); code != 1 || !strings.Contains(errOut, "tool.borrow_network") {
		t.Fatalf("a network tool without edit_network_tools: %d %s", code, errOut)
	}
	mustWrite(t, filepath.Join(f.state, config.FileName), "[routing]\nedit_tools = [\"context7\"]\nedit_network_tools = true\n", 0o600)
	code, out, errOut := f.run(t, "edit", "add a helper", "--vendor", "codex")
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errOut)
	}
	var child fakecli.Record
	for _, r := range fakecli.Records(t, f.record) {
		if r.Mode == "headless" {
			child = r
		}
	}
	args := strings.Join(child.Args, " ")
	run := filepath.Join(f.state, "run")
	if !strings.Contains(args, `-c mcp_servers.context7.command="`+testBin+`"`) ||
		!strings.Contains(args, `-c mcp_servers.context7.default_tools_approval_mode="approve"`) ||
		!strings.Contains(args, `mcp_servers.context7.args=["tool", "exec", "`+run+`/borrow-`) {
		t.Errorf("child args: %s", args)
	}
	if des, _ := os.ReadDir(run); slices.ContainsFunc(des, func(de os.DirEntry) bool { return strings.HasSuffix(de.Name(), ".tools") }) {
		t.Errorf("a launch folder outlived the child: %v", des)
	}

	// --tool names one routing.edit_tools does not list.
	if code, _, errOut := f.run(t, "edit", "add a helper", "--vendor", "codex", "--tool", "codegraph"); code != 1 || !strings.Contains(errOut, "tool.borrow_unlisted") {
		t.Errorf("unlisted: %d %s", code, errOut)
	}
	// A review never gets one.
	if code, _, errOut := f.run(t, "review", "look", "--vendor", "codex", "--tool", "context7"); code != 2 || !strings.Contains(errOut, "tool.borrow_kind") {
		t.Errorf("review: %d %s", code, errOut)
	}
}

// TestBorrowToolsRefusals (T6709): what the launch refuses, a child
// refuses too, and the vendor is passed over: Grok (gated), Codex
// without -c, a tool off by the repo's tools_off, one with no MCP
// server, a name the CLI already loads (fail closed), and an unknown
// name.
func TestBorrowToolsRefusals(t *testing.T) {
	f := toolsFixture(t, `["context7"]`)
	gitRepo(t, f.repo)
	wire := borrowTools(f.state, true)
	ctx := context.Background()
	codex := borrow.CLI{Caps: []string{probe.CapConfigOverride}}
	check := func(name string, v adapter.Vendor, cli borrow.CLI, names []string, want diag.Code) {
		t.Helper()
		servers, done, err := wire(ctx, v, cli, names, f.repo)
		var te *borrow.ToolError
		if !errors.As(err, &te) || te.Code != want || servers != nil || done != nil {
			t.Errorf("%s: %v %v, want %v", name, servers, err, want)
		}
	}
	check("grok", adapter.Grok, borrow.CLI{}, []string{"context7"}, diag.CodeToolUnverified)
	check("codex without -c", adapter.Codex, borrow.CLI{}, []string{"context7"}, diag.CodeToolNoFlag)
	check("unknown", adapter.Codex, codex, []string{"nope"}, diag.CodeToolUnknown)
	check("skill only", adapter.Codex, codex, []string{"caveman"}, diag.CodeToolScope)
	// M78-2: a network tool, routing.edit_network_tools off.
	if _, _, err := borrowTools(f.state, false)(ctx, adapter.Claude, borrow.CLI{}, []string{"context7"}, f.repo); !strings.Contains(fmt.Sprint(err), "tool.borrow_network") {
		t.Errorf("network: %v", err)
	}
	// M78-3: Claude's managed-mcp.json still counts for a child.
	managed := filepath.Join(t.TempDir(), "managed-mcp.json")
	mustWrite(t, managed, `{"mcpServers":{"context7":{}}}`, 0o600)
	oldManaged := claudeManagedMCP
	claudeManagedMCP = []string{managed}
	check("claude managed clash", adapter.Claude, borrow.CLI{}, []string{"context7"}, diag.CodeToolNameClash)
	claudeManagedMCP = oldManaged

	// Claude reads no other MCP config, so a name it loads elsewhere is no
	// clash; Codex's project config is, and fails closed.
	mustWrite(t, filepath.Join(f.repo, ".codex", "config.toml"), "[mcp_servers.context7]\ncommand = \"x\"\n", 0o600)
	check("codex clash", adapter.Codex, codex, []string{"context7"}, diag.CodeToolNameClash)
	servers, done, err := wire(ctx, adapter.Claude, borrow.CLI{}, []string{"context7"}, f.repo)
	if err != nil || len(servers) != 1 || servers[0].Name != "context7" || servers[0].Command != testBin {
		t.Fatalf("claude: %+v %v", servers, err)
	}
	if fi, err := os.Stat(servers[0].Cwd); err != nil || fi.Mode().Perm() != 0o700 || filepath.Dir(servers[0].Cwd) != filepath.Join(f.state, "run") {
		t.Errorf("launch folder %s: %v %v", servers[0].Cwd, fi, err)
	}
	done()
	if _, err := os.Stat(servers[0].Cwd); !os.IsNotExist(err) {
		t.Errorf("done left %s: %v", servers[0].Cwd, err)
	}

	mustWrite(t, filepath.Join(f.repo, ".aggy", "config.toml"), `tools_off = ["context7"]`, 0o600)
	check("repo off", adapter.Claude, borrow.CLI{}, []string{"context7"}, diag.CodeToolRepoOff)
}

// TestBorrowToolsUserNetwork (R80-2): a user definition without the
// network key is taken to reach the network, so an edit child refuses
// it without routing.edit_network_tools; one added saying network =
// false is wired. aggy tool add shows which.
func TestBorrowToolsUserNetwork(t *testing.T) {
	f := toolsFixture(t, `["demo"]`)
	gitRepo(t, f.repo)
	// The fixture sets it empty (t.Setenv restores it); tool add refuses
	// while it is set at all.
	_ = os.Unsetenv("AGGY_DELEGATED")
	ctx := context.Background()
	add := func(def, shows string) {
		t.Helper()
		_, _, _ = toolRun("", false, "remove", "demo")
		src := filepath.Join(t.TempDir(), "demo.toml")
		writeAt(t, src, def, 0o644)
		code, out, errOut := toolRun("", true, "add", src, "--yes")
		if code != 0 || !strings.Contains(out, shows) {
			t.Fatalf("add: %d %q %q", code, out, errOut)
		}
	}
	add(demoTool, "network: reaches the network")
	if _, _, err := borrowTools(f.state, false)(ctx, adapter.Claude, borrow.CLI{}, []string{"demo"}, f.repo); !strings.Contains(fmt.Sprint(err), "tool.borrow_network") {
		t.Errorf("a user tool without the network key: %v", err)
	}
	add("network = false\n"+demoTool, "network: none")
	servers, done, err := borrowTools(f.state, false)(ctx, adapter.Claude, borrow.CLI{}, []string{"demo"}, f.repo)
	if err != nil || len(servers) != 1 || servers[0].Name != "demo" {
		t.Fatalf("network = false: %+v %v", servers, err)
	}
	done()
}

// TestBorrowToolsSkillChanged (L82-3): an edit child refuses a tool
// whose linked skill folder changed since it was first linked
// (tool.skill_changed, R80-3), as a ToolError, so the borrow passes to
// the next vendor.
func TestBorrowToolsSkillChanged(t *testing.T) {
	f := toolsFixture(t, `["demo"]`)
	gitRepo(t, f.repo)
	_ = os.Unsetenv("AGGY_DELEGATED") // tool add and on refuse while it is set
	ctx := context.Background()
	src := filepath.Join(t.TempDir(), "demo.toml")
	writeAt(t, src, "network = false\n"+demoTool+"\n[[skill]]\nvendors = [\"claude\"]\npath = \"~/.aggy/skills/demo\"\n", 0o644)
	if code, _, errOut := toolRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatalf("add: %d %q", code, errOut)
	}
	skill := filepath.Join(f.state, "skills", "demo", "SKILL.md")
	writeAt(t, skill, "---\nname: demo\ndescription: short\n---\n", 0o644)
	if code, _, errOut := toolRun("", false, "on", "demo", "--vendor", "claude"); code != 0 {
		t.Fatalf("on: %d %q", code, errOut)
	}
	wire := borrowTools(f.state, false)
	servers, done, err := wire(ctx, adapter.Claude, borrow.CLI{}, []string{"demo"}, f.repo)
	if err != nil || len(servers) != 1 {
		t.Fatalf("before the change: %+v %v", servers, err)
	}
	done()
	writeAt(t, skill, "---\nname: demo\ndescription: changed\n---\n", 0o644)
	servers, done, err = wire(ctx, adapter.Claude, borrow.CLI{}, []string{"demo"}, f.repo)
	var te *borrow.ToolError
	if !errors.As(err, &te) || te.Code != diag.CodeToolSkillChanged || servers != nil || done != nil {
		t.Errorf("a changed skill: %+v %v", servers, err)
	}
}
