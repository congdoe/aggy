package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/tooldef"
)

// wireHome is toolHome with aggy's own path set to testBin, the shim
// every server starts through.
func wireHome(t *testing.T) (src, dest string) {
	t.Helper()
	src, dest = toolHome(t, demoTool)
	oldBin, oldPath, oldCheck := hookBin, pathAggy, checkTrusted
	hookBin = func() (string, error) { return testBin, nil }
	pathAggy = func() (string, error) { return "", os.ErrNotExist }
	checkTrusted = func(string) error { return nil }
	t.Cleanup(func() { hookBin, pathAggy, checkTrusted = oldBin, oldPath, oldCheck })
	return src, dest
}

// wireOnce runs addTools for v in dir cwd and returns the request, the
// warnings, and the launch folder it used.
func wireOnce(t *testing.T, v adapter.Vendor, caps []string, cwd string) (adapter.InteractiveRequest, []string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "launch")
	var req adapter.InteractiveRequest
	warns := addTools(context.Background(), v, caps, cwd, dir, nil, &req)
	return req, warns, dir
}

type claudeMCPFile struct {
	MCPServers map[string]struct {
		Command string
		Args    []string
	} `json:"mcpServers"`
}

func readClaudeMCP(t *testing.T, f string) claudeMCPFile {
	t.Helper()
	var doc claudeMCPFile
	b, err := os.ReadFile(f)
	if err != nil || json.Unmarshal(b, &doc) != nil {
		t.Fatalf("mcp.json: %v %s", err, b)
	}
	return doc
}

// shimTail is the argv a shim entry runs, after "--".
func shimTail(args []string) string {
	for i, a := range args {
		if a == "--" {
			return strings.Join(args[i+1:], " ")
		}
	}
	return ""
}

// TestAddToolsSession: aggy run --tool turns a tool on for one session,
// changes no setting, and never beats the repo's tools_off; an unknown
// name is a warning.
func TestAddToolsSession(t *testing.T) {
	src, _ := wireHome(t)
	if code, _, errOut := toolRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatalf("add: %d %q", code, errOut)
	}
	repo := switchRepo(t, "")
	var req adapter.InteractiveRequest
	warns := addTools(context.Background(), adapter.Claude, nil, repo, filepath.Join(t.TempDir(), "l"), []string{"demo", "nope"}, &req)
	b, _ := os.ReadFile(req.MCPConfigFile)
	if len(warns) != 1 || !strings.Contains(warns[0], "tool.unknown") || !strings.Contains(string(b), `"demo"`) {
		t.Errorf("session: %q\n%s", warns, b)
	}
	if _, out, _ := toolRun("", false, "status"); !strings.HasPrefix(statusLine(t, out, "demo"), "demo off off off") {
		t.Errorf("--tool changed a setting:\n%s", out)
	}
	writeAt(t, filepath.Join(repo, ".aggy", "config.toml"), `tools_off = ["demo"]`, 0o600)
	req = adapter.InteractiveRequest{}
	warns = addTools(context.Background(), adapter.Claude, nil, repo, filepath.Join(t.TempDir(), "l"), []string{"demo"}, &req)
	if req.MCPConfigFile != "" || len(warns) != 1 || !strings.Contains(warns[0], "tool.repo_off") {
		t.Errorf("--tool against the repo's tools_off: %+v %q", req, warns)
	}
}

// TestAddToolsSessionChanged (W76-1): --tool wires a user tool only
// while its file is the one aggy tool add showed, as aggy tool on
// checks (S63-16); a file aggy tool add never recorded is refused too.
func TestAddToolsSessionChanged(t *testing.T) {
	src, dest := wireHome(t)
	if code, _, errOut := toolRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatalf("add: %d %q", code, errOut)
	}
	repo := switchRepo(t, "")
	writeAt(t, dest, strings.Replace(demoTool, `"--stdio"`, `"--evil"`, 1), 0o600)
	if code, _, errOut := toolRun("", false, "on", "demo"); code == 0 || !strings.Contains(errOut, "tool.changed_since_add") {
		t.Fatalf("aggy tool on took the edited file: %d %q", code, errOut)
	}
	var req adapter.InteractiveRequest
	warns := addTools(context.Background(), adapter.Claude, nil, repo, filepath.Join(t.TempDir(), "l"), []string{"demo"}, &req)
	if req.MCPConfigFile != "" || len(warns) != 1 || !strings.Contains(warns[0], "tool.changed_since_add") {
		t.Errorf("--tool after an edit: %+v %q", req, warns)
	}

	// A file put in the tools folder by hand, never added.
	if code, _, errOut := toolRun("", false, "remove", "demo"); code != 0 {
		t.Fatalf("remove: %d %q", code, errOut)
	}
	writeAt(t, dest, demoTool, 0o600)
	req = adapter.InteractiveRequest{}
	warns = addTools(context.Background(), adapter.Claude, nil, repo, filepath.Join(t.TempDir(), "l"), []string{"demo"}, &req)
	if req.MCPConfigFile != "" || len(warns) != 1 || !strings.Contains(warns[0], "no record") {
		t.Errorf("--tool with no add record: %+v %q", req, warns)
	}
}

// TestSessionToolsIndexError (R76-F2): an index that cannot be opened
// refuses every user --tool with its error, not only the first.
func TestSessionToolsIndexError(t *testing.T) {
	_, dest := wireHome(t)
	writeAt(t, dest, demoTool, 0o600)
	writeAt(t, filepath.Join(filepath.Dir(dest), "demo2.toml"), strings.NewReplacer(`name = "demo"`, `name = "demo2"`, "DEMO_", "DEMO2_").Replace(demoTool), 0o600)
	db, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	writeAt(t, db, "not a database", 0o600)
	r, _, err := tooldef.Load(os.Getenv(home.Env))
	if err != nil {
		t.Fatal(err)
	}
	_, warns := sessionTools(context.Background(), switchboard.Board{Tools: r.List()}, adapter.Claude, "", []string{"demo", "demo2"},
		func(tool string, code diag.Code, _, why string) string { return tool + " " + code.String() + " " + why })
	if len(warns) != 2 {
		t.Fatalf("warns %q", warns)
	}
	for _, w := range warns {
		if !strings.Contains(w, "the index could not be read") {
			t.Errorf("%q", w)
		}
	}
}

// TestAddToolsUnaddedRow (R76-F1): an "on" recorded for a user file put
// in the tools folder by hand (as aggy tool on took it before) wires
// nothing at launch, for any vendor, and status says why.
func TestAddToolsUnaddedRow(t *testing.T) {
	_, dest := wireHome(t)
	writeAt(t, dest, demoTool, 0o600)
	repo := switchRepo(t, "")
	r, _, err := tooldef.Load(os.Getenv(home.Env))
	if err != nil {
		t.Fatal(err)
	}
	demo, _ := r.Get("demo")
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	err = s.PutToolSwitch(ctx, store.ToolSwitch{Tool: "demo", On: true, Digest: demo.Digest, At: time.Now()})
	_ = s.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range adapter.Vendors() {
		req, warns, dir := wireOnce(t, v, []string{probe.CapConfigOverride, probe.CapSettings}, repo)
		if req.MCPConfigFile != "" || req.AgentFile != "" || req.MCP != nil || req.Hooks != nil || len(warns) != 0 || exists(t, dir) {
			t.Errorf("%s: %+v %q", v, req, warns)
		}
	}
	t.Chdir(repo)
	if _, out, _ := toolRun("", false, "status", "demo"); !strings.Contains(out, switchboard.NotAdded) {
		t.Errorf("status:\n%s", out)
	}
}

// TestAddToolsSessionOff (W76-2, W76-3): a --tool that cannot be on, or
// has nothing to wire, says so; Grok stays gated for one too.
func TestAddToolsSessionOff(t *testing.T) {
	src, _ := wireHome(t)
	writeAt(t, src, strings.Replace(demoTool, `scope = "global"`, `scope = "repo"`, 1), 0o600)
	if code, _, errOut := toolRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatalf("add: %d %q", code, errOut)
	}
	repo := switchRepo(t, "")
	wire := func(v adapter.Vendor, cwd string, session ...string) (adapter.InteractiveRequest, []string, string) {
		dir := filepath.Join(t.TempDir(), "l")
		var req adapter.InteractiveRequest
		return req, addTools(context.Background(), v, nil, cwd, dir, session, &req), dir
	}

	// Grok: context7 has a Grok server, but the wiring is gated.
	req, warns, dir := wire(adapter.Grok, repo, "context7")
	if req.AgentFile != "" || len(warns) != 1 || !strings.Contains(warns[0], "tool.vendor_unverified") || exists(t, dir) {
		t.Errorf("grok: %+v %q", req, warns)
	}
	// demo has only a hook for Grok, which takes none for one session.
	req, warns, _ = wire(adapter.Grok, repo, "demo")
	if req.AgentFile != "" || req.Hooks != nil || len(warns) != 1 || !strings.Contains(warns[0], "tool.no_launch_flag") {
		t.Errorf("grok demo: %+v %q", req, warns)
	}
	// caveman is a skill only: on, but a session wires none (W76-3).
	req, warns, _ = wire(adapter.Claude, repo, "caveman")
	if req.MCPConfigFile != "" || req.Hooks != nil || len(warns) != 1 || !strings.Contains(warns[0], "a skill follows aggy tool on") {
		t.Errorf("claude caveman: %+v %q", req, warns)
	}
	// A per-repo tool: wired in a repo, refused outside one.
	if req, warns, _ = wire(adapter.Claude, repo, "demo"); req.MCPConfigFile == "" || len(warns) != 0 {
		t.Errorf("per-repo in a repo: %+v %q", req, warns)
	}
	req, warns, _ = wire(adapter.Claude, t.TempDir(), "demo")
	if req.MCPConfigFile != "" || len(warns) != 1 || !strings.Contains(warns[0], "tool.not_applicable") || !strings.Contains(warns[0], "outside a repo") {
		t.Errorf("per-repo outside a repo: %+v %q", req, warns)
	}
}

// TestAddToolsHooks: RTK's Claude PreToolUse hook is wired through aggy
// hook-tool in the session's --settings layer only when the probe found
// --settings; off, it is gone.
func TestAddToolsHooks(t *testing.T) {
	isolate(t)
	old := toolHookBin
	toolHookBin = func() (string, error) { return "/opt/bin/aggy", nil }
	defer func() { toolHookBin = old }()
	repo := switchRepo(t, "")
	if code, _, errOut := toolRun("", false, "on", "rtk"); code != 0 {
		t.Fatalf("on: %d %q", code, errOut)
	}
	req, warns, _ := wireOnce(t, adapter.Claude, []string{probe.CapSettings}, repo)
	g := req.Hooks["PreToolUse"]
	if len(warns) != 0 || len(g) != 1 || g[0].Matcher != "Bash" || !strings.Contains(g[0].Hooks[0].Command, "/opt/bin/aggy hook-tool --vendor claude rtk 0 ") {
		t.Errorf("hooks %+v %q", req.Hooks, warns)
	}
	if req.Deny != nil {
		t.Errorf("no policy, yet deny rules: %q", req.Deny)
	}
	// H78-1: with the policy on, RTK's rewrite prefix adds the policy's
	// rules again with "rtk " in front (golden), and the shared list is
	// left as it was.
	policy := slices.Clone(parity.ClaudeDeny)
	req = adapter.InteractiveRequest{Deny: parity.ClaudeDeny}
	addTools(context.Background(), adapter.Claude, []string{probe.CapSettings}, repo, filepath.Join(t.TempDir(), "l"), nil, &req)
	want := slices.Concat(parity.ClaudeDeny, []string{
		"Bash(rtk *.env*)", "Bash(rtk *.env.*)", "Bash(rtk *.pem*)", "Bash(rtk *.key*)",
		"Bash(rtk *id_rsa*)", "Bash(rtk *id_dsa*)", "Bash(rtk *id_ecdsa*)", "Bash(rtk *id_ed25519*)",
		"Bash(rtk git push * main)", "Bash(rtk git push * main *)", "Bash(rtk git push * *:main)", "Bash(rtk git push * *:main *)",
		"Bash(rtk git push * refs/heads/main)", "Bash(rtk git push * refs/heads/main *)",
		"Bash(rtk git push * *:refs/heads/main)", "Bash(rtk git push * *:refs/heads/main *)",
		"Bash(rtk git push --force*)", "Bash(rtk git push -f*)", "Bash(rtk git push * --force*)", "Bash(rtk git push * -f*)", "Bash(rtk git push * +*)",
		"Bash(rtk rm -rf*)", "Bash(rtk rm -fr*)", "Bash(rtk rm -Rf*)", "Bash(rtk rm -fR*)",
		"Bash(rtk rm -r -f*)", "Bash(rtk rm -f -r*)", "Bash(rtk rm -R -f*)", "Bash(rtk rm -f -R*)",
		"Bash(rtk rm -r --force*)", "Bash(rtk rm -R --force*)", "Bash(rtk rm -f --recursive*)", "Bash(rtk rm --recursive -f*)",
		"Bash(rtk rm --force -r*)", "Bash(rtk rm --force -R*)", "Bash(rtk rm --recursive --force*)", "Bash(rtk rm --force --recursive*)",
		"Bash(rtk git reset --hard*)", "Bash(rtk git reset * --hard*)", "Bash(rtk git clean *)",
	})
	if !slices.Equal(req.Deny, want) || !slices.Equal(parity.ClaudeDeny, policy) {
		t.Errorf("deny = %q", req.Deny)
	}
	if req, warns, _ := wireOnce(t, adapter.Claude, nil, repo); req.Hooks != nil || len(warns) != 1 || !strings.Contains(warns[0], "tool.no_launch_flag") {
		t.Errorf("without --settings: %+v %q", req.Hooks, warns)
	}
	if req, warns, _ := wireOnce(t, adapter.Codex, []string{probe.CapConfigOverride}, repo); req.Hooks != nil || len(warns) != 0 {
		t.Errorf("codex has no rtk hook: %+v %q", req.Hooks, warns)
	}
	if code, _, _ := toolRun("", false, "off", "rtk"); code != 0 {
		t.Fatal("off")
	}
	if req, _, _ := wireOnce(t, adapter.Claude, []string{probe.CapSettings}, repo); req.Hooks != nil {
		t.Errorf("off: %+v", req.Hooks)
	}
}

// TestAddTools: the tools that are on reach each CLI's launch as its own
// form (a Claude MCP file, Codex -c servers), each started through aggy
// tool exec; an off tool leaves no flag and no file; Grok gets none
// (W71-4).
func TestAddTools(t *testing.T) {
	_, dest := wireHome(t)
	addedAt(t, dest, strings.Replace(demoTool, `"--stdio"`, `"--root", "{repo}"`, 1))
	repo := switchRepo(t, "")
	all := []string{probe.CapConfigOverride}

	// Nothing on: no flag, no file, no folder.
	for _, v := range adapter.Vendors() {
		req, warns, dir := wireOnce(t, v, all, repo)
		if req.MCPConfigFile != "" || req.AgentFile != "" || req.MCP != nil || len(warns) != 0 || exists(t, dir) {
			t.Errorf("%s with nothing on: %+v %q", v, req, warns)
		}
	}

	for _, args := range [][]string{{"on", "context7"}, {"on", "demo"}} {
		if code, _, errOut := toolRun("", false, args...); code != 0 {
			t.Fatalf("%v: %d %q", args, code, errOut)
		}
	}
	req, warns, dir := wireOnce(t, adapter.Claude, nil, repo)
	if len(warns) != 0 || req.MCPConfigFile != filepath.Join(dir, "mcp.json") {
		t.Fatalf("claude: %+v %q", req, warns)
	}
	doc := readClaudeMCP(t, req.MCPConfigFile)
	demo := doc.MCPServers["demo"]
	if len(doc.MCPServers) != 2 || demo.Command != testBin || doc.MCPServers["context7"].Command != testBin ||
		strings.Join(demo.Args[:4], " ") != "tool exec "+dir+" demo" || demo.Args[5] != repo ||
		shimTail(demo.Args) != "demo serve --root "+repo || shimTail(doc.MCPServers["context7"].Args) != "context7-mcp" {
		t.Errorf("mcp.json = %+v", doc)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("launch folder: %v %v", fi, err)
	}

	req, warns, dir = wireOnce(t, adapter.Grok, nil, repo)
	// Two warnings: demo's Grok hook is never wired per session, and Grok
	// gets no MCP server (W71-4).
	if req.AgentFile != "" || len(warns) != 2 || !strings.Contains(warns[0], "PostToolUse hook is not wired") ||
		!strings.Contains(warns[1], "tool.vendor_unverified") || exists(t, dir) {
		t.Errorf("grok: %+v %q", req, warns)
	}

	req, warns, _ = wireOnce(t, adapter.Codex, nil, repo)
	if req.MCP != nil || len(warns) != 1 || !strings.Contains(warns[0], "tool.no_launch_flag") {
		t.Errorf("codex without -c: %+v %q", req.MCP, warns)
	}
	req, warns, dir = wireOnce(t, adapter.Codex, all, repo)
	if len(warns) != 0 || len(req.MCP) != 2 || req.MCP[1].Name != "demo" || req.MCP[1].Cwd != dir || req.MCP[1].Command != testBin ||
		shimTail(req.MCP[1].Args) != "demo serve --root "+repo || strings.Join(req.MCP[1].EnvVars, ",") != "DEMO_HOME" {
		t.Errorf("codex: %+v %q", req.MCP, warns)
	}

	// Off really means off, and the repo's tools_off wins.
	writeAt(t, filepath.Join(repo, ".aggy", "config.toml"), `tools_off = ["demo"]`, 0o600)
	req, _, _ = wireOnce(t, adapter.Codex, all, repo)
	if len(req.MCP) != 1 || req.MCP[0].Name != "context7" {
		t.Errorf("tools_off: %+v", req.MCP)
	}
	if code, _, _ := toolRun("", false, "off", "context7"); code != 0 {
		t.Fatal("off")
	}
	for _, v := range adapter.Vendors() {
		req, warns, dir := wireOnce(t, v, all, repo)
		if req.MCPConfigFile != "" || req.AgentFile != "" || req.MCP != nil || len(warns) != 0 || exists(t, dir) {
			t.Errorf("%s after off: %+v %q", v, req, warns)
		}
	}
}

// TestAddToolsGrokGated: W71-4. tool on --vendor grok is refused, and
// the --agent code behind the gate still renders the agent file.
func TestAddToolsGrokGated(t *testing.T) {
	wireHome(t)
	repo := switchRepo(t, "")
	if code, _, errOut := toolRun("", false, "on", "context7", "--vendor", "grok"); code != 1 || !strings.Contains(errOut, "tool.vendor_unverified") {
		t.Errorf("on --vendor grok: %d %q", code, errOut)
	}
	if code, _, errOut := toolRun("", false, "on", "context7"); code != 0 {
		t.Fatalf("on: %q", errOut)
	}
	grokToolsVerified = true
	t.Cleanup(func() { grokToolsVerified = false })
	req, warns, dir := wireOnce(t, adapter.Grok, nil, repo)
	b, _ := os.ReadFile(req.AgentFile)
	if len(warns) != 0 || !strings.Contains(string(b), `{"context7":{"command":"`+testBin+`","args":["tool","exec","`+dir+`","context7"`) {
		t.Errorf("grok behind the gate: %q\n%s", warns, b)
	}
}

// TestAddToolsClash: a name the CLI already loads is refused, never
// merged, and the warning names where it is set (W71-9). Claude's
// .mcp.json counts from cwd up to / and its local scope under any
// project (W71-5); Codex's every layer, disabled servers and profiles
// included (W71-3). A file that cannot be parsed refuses every tool.
func TestAddToolsClash(t *testing.T) {
	_, dest := wireHome(t)
	addedAt(t, dest, demoTool)
	h := os.Getenv("HOME")
	parent := t.TempDir()
	repo := filepath.Join(parent, "app")
	runGit(t, parent, "init", "-q", "app")
	sub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	all := []string{probe.CapConfigOverride}
	for _, args := range [][]string{{"on", "context7"}, {"on", "demo"}} {
		if code, _, errOut := toolRun("", false, args...); code != 0 {
			t.Fatalf("%v: %q", args, errOut)
		}
	}
	claudeJSON := filepath.Join(h, ".claude.json")
	managed := filepath.Join(t.TempDir(), "managed-mcp.json")
	oldManaged := claudeManagedMCP
	claudeManagedMCP = []string{managed, filepath.Join(t.TempDir(), "absent.json")}
	t.Cleanup(func() { claudeManagedMCP = oldManaged })
	for _, c := range []struct {
		name, file, text, want string
	}{
		{"user scope", claudeJSON, `{"mcpServers":{"context7":{"command":"mine"}}}`, claudeJSON},
		{"another project's local scope", claudeJSON, `{"projects":{"/elsewhere/app":{"mcpServers":{"context7":{"command":"x"}}}}}`, "local scope of /elsewhere/app"},
		{"the repo's .mcp.json", filepath.Join(repo, ".mcp.json"), `{"mcpServers":{"context7":{}}}`, filepath.Join(repo, ".mcp.json")},
		{"a .mcp.json above the repo", filepath.Join(parent, ".mcp.json"), `{"mcpServers":{"context7":{}}}`, filepath.Join(parent, ".mcp.json")},
		{"the managed-mcp.json", managed, `{"mcpServers":{"context7":{}}}`, managed},
	} {
		t.Run("claude "+c.name, func(t *testing.T) {
			writeAt(t, c.file, c.text, 0o600)
			defer func() { _ = os.Remove(c.file) }()
			req, warns, _ := wireOnce(t, adapter.Claude, nil, sub)
			if len(warns) != 1 || !strings.Contains(warns[0], "tool context7 not wired") || !strings.Contains(warns[0], "tool.name_clash") ||
				!strings.Contains(warns[0], c.want) {
				t.Fatalf("warns %q", warns)
			}
			if doc := readClaudeMCP(t, req.MCPConfigFile); len(doc.MCPServers) != 1 || doc.MCPServers["demo"].Command == "" {
				t.Errorf("wired %+v", doc)
			}
		})
	}
	codexHome := filepath.Join(h, ".codex")
	for _, c := range []struct {
		name, file, text string
	}{
		{"user, disabled", filepath.Join(codexHome, "config.toml"), "[mcp_servers.context7]\ncommand = \"x\"\nenabled = false\n"},
		{"user, in a profile", filepath.Join(codexHome, "config.toml"), "[profiles.work.mcp_servers.context7]\ncommand = \"x\"\n"},
		{"selected profile file", filepath.Join(codexHome, "work.config.toml"), "[mcp_servers.context7]\ncommand = \"x\"\n"},
		{"project layer", filepath.Join(repo, ".codex", "config.toml"), "[mcp_servers.context7]\ncommand = \"x\"\n"},
		{"project layer below the root", filepath.Join(sub, ".codex", "config.toml"), "[mcp_servers.context7]\ncommand = \"x\"\n"},
	} {
		t.Run("codex "+c.name, func(t *testing.T) {
			if strings.HasSuffix(c.file, "work.config.toml") {
				writeAt(t, filepath.Join(codexHome, "config.toml"), "profile = \"work\"\n", 0o600)
				defer func() { _ = os.Remove(filepath.Join(codexHome, "config.toml")) }()
			}
			writeAt(t, c.file, c.text, 0o600)
			defer func() { _ = os.Remove(c.file) }()
			req, warns, _ := wireOnce(t, adapter.Codex, all, sub)
			if len(warns) != 1 || !strings.Contains(warns[0], "tool.name_clash") || !strings.Contains(warns[0], c.file) ||
				len(req.MCP) != 1 || req.MCP[0].Name != "demo" {
				t.Errorf("%+v %q", req.MCP, warns)
			}
		})
	}
	// A .mcp.json above the repo that this user cannot read is no clash;
	// in the repo it still refuses every tool.
	for f, wired := range map[string]bool{filepath.Join(parent, ".mcp.json"): true, filepath.Join(repo, ".mcp.json"): false, managed: true} {
		writeAt(t, f, `{"mcpServers":{"context7":{}}}`, 0o000)
		req, warns, _ := wireOnce(t, adapter.Claude, nil, sub)
		if got := req.MCPConfigFile != ""; got != wired || wired && len(warns) != 0 {
			t.Errorf("unreadable %s: wired %v, warns %q", f, got, warns)
		}
		_ = os.Remove(f)
	}
	// Fail closed: a file that cannot be parsed wires nothing.
	for v, f := range map[adapter.Vendor]string{
		adapter.Claude: filepath.Join(parent, ".mcp.json"),
		adapter.Codex:  filepath.Join(repo, ".codex", "config.toml"),
	} {
		writeAt(t, f, "{not", 0o600)
		req, warns, dir := wireOnce(t, v, all, sub)
		if req.MCPConfigFile != "" || req.MCP != nil || len(warns) != 1 || !strings.Contains(warns[0], "tool.wire_failed") || exists(t, dir) {
			t.Errorf("%s with %s unparsable: %+v %q", v, f, req, warns)
		}
		_ = os.Remove(f)
	}
	writeAt(t, managed, "{not", 0o600)
	if req, warns, _ := wireOnce(t, adapter.Claude, nil, sub); req.MCPConfigFile != "" || len(warns) != 1 || !strings.Contains(warns[0], "tool.wire_failed") {
		t.Errorf("managed-mcp.json unparsable: %+v %q", req, warns)
	}
}

// TestAddToolsLaunchDir: W71-10, a launch folder that is not aggy's own
// private one is refused, and no file is written through a symlink.
func TestAddToolsLaunchDir(t *testing.T) {
	wireHome(t)
	repo := switchRepo(t, "")
	if code, _, errOut := toolRun("", false, "on", "context7"); code != 0 {
		t.Fatal(errOut)
	}
	open := filepath.Join(t.TempDir(), "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{open, link} {
		var req adapter.InteractiveRequest
		warns := addTools(context.Background(), adapter.Claude, nil, repo, dir, nil, &req)
		if req.MCPConfigFile != "" || len(warns) != 1 || !strings.Contains(warns[0], "tool.wire_failed") {
			t.Errorf("%s: %+v %q", dir, req, warns)
		}
	}
	if exists(t, filepath.Join(target, "mcp.json")) || exists(t, filepath.Join(open, "mcp.json")) {
		t.Error("a file was written")
	}
}

// TestAddToolsRecordText: W71-11, a refusal's record holds the code's
// registry text and a fixed class, never the error's own text.
func TestAddToolsRecordText(t *testing.T) {
	freshRun(t)
	wireHome(t)
	const canary = "wire-canary-ocelot"
	parent := filepath.Join(t.TempDir(), canary)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, parent, "init", "-q", "app")
	repo := filepath.Join(parent, "app")
	if code, _, errOut := toolRun("", false, "on", "context7"); code != 0 {
		t.Fatal(errOut)
	}
	writeAt(t, filepath.Join(parent, ".mcp.json"), "{"+canary, 0o600)
	_, warns, _ := wireOnce(t, adapter.Claude, nil, repo)
	if len(warns) != 1 || !strings.Contains(warns[0], canary) {
		t.Fatalf("warns %q", warns)
	}
	text := allDiagText(t, os.Getenv("AGGY_HOME"))
	info, _ := diag.LookupCode(diag.CodeToolWireFailed)
	if strings.Contains(text, canary) || !strings.Contains(text, info.Meaning) || !strings.Contains(text, `"class":"names"`) {
		t.Errorf("diag.db: %s", text)
	}
}
