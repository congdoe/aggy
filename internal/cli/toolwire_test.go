package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/probe"
)

// wireOnce runs addTools for v in dir cwd and returns the request, the
// warnings, and the launch folder it used.
func wireOnce(t *testing.T, v adapter.Vendor, caps []string, cwd string) (adapter.InteractiveRequest, []string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "launch")
	var req adapter.InteractiveRequest
	warns := addTools(context.Background(), v, caps, cwd, dir, nil, &req)
	return req, warns, dir
}

// TestAddTools: the tools that are on reach each CLI's launch as its own
// form (a Claude MCP file, Grok agent file, Codex -c servers); an off
// tool leaves no flag and no file; a name the user's config already has
// is refused, never merged.
func TestAddTools(t *testing.T) {
	_, dest := toolHome(t, demoTool)
	writeAt(t, dest, strings.Replace(demoTool, `"--stdio"`, `"--root", "{repo}"`, 1), 0o600)
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
	var doc struct {
		MCPServers map[string]struct {
			Command string
			Args    []string
		} `json:"mcpServers"`
	}
	b, err := os.ReadFile(req.MCPConfigFile)
	if err != nil || json.Unmarshal(b, &doc) != nil {
		t.Fatalf("mcp.json: %v %s", err, b)
	}
	if len(doc.MCPServers) != 2 || doc.MCPServers["context7"].Command != "context7-mcp" ||
		strings.Join(doc.MCPServers["demo"].Args, " ") != "serve --root "+repo {
		t.Errorf("mcp.json = %s", b)
	}

	req, warns, dir = wireOnce(t, adapter.Grok, nil, repo)
	b, _ = os.ReadFile(req.AgentFile)
	if len(warns) != 0 || !strings.Contains(string(b), `- {"context7":{"command":"context7-mcp","args":[],"cwd":"`+dir+`"}}`) ||
		strings.Contains(string(b), "demo") {
		t.Errorf("grok (demo has no grok MCP part): %q\n%s", warns, b)
	}

	req, warns, _ = wireOnce(t, adapter.Codex, nil, repo)
	if req.MCP != nil || len(warns) != 1 || !strings.Contains(warns[0], "tool.no_launch_flag") {
		t.Errorf("codex without -c: %+v %q", req.MCP, warns)
	}
	req, warns, dir = wireOnce(t, adapter.Codex, all, repo)
	if len(warns) != 0 || len(req.MCP) != 2 || req.MCP[1].Name != "demo" || req.MCP[1].Cwd != dir ||
		strings.Join(req.MCP[1].EnvVars, ",") != "DEMO_HOME" {
		t.Errorf("codex: %+v %q", req.MCP, warns)
	}

	// The user's own context7 is never replaced or merged into.
	writeAt(t, filepath.Join(os.Getenv("HOME"), ".claude.json"), `{"mcpServers":{"context7":{"command":"mine"}}}`, 0o600)
	req, warns, _ = wireOnce(t, adapter.Claude, nil, repo)
	b, _ = os.ReadFile(req.MCPConfigFile)
	if len(warns) != 1 || !strings.Contains(warns[0], "tool context7 not wired") || !strings.Contains(warns[0], "tool.name_clash") ||
		strings.Contains(string(b), "context7") || !strings.Contains(string(b), "demo") {
		t.Errorf("clash: %q\n%s", warns, b)
	}
	// And a repo's .mcp.json server of that name.
	writeAt(t, filepath.Join(repo, ".mcp.json"), `{"mcpServers":{"demo":{"command":"x"}}}`, 0o600)
	req, warns, dir = wireOnce(t, adapter.Claude, nil, repo)
	if len(warns) != 2 || req.MCPConfigFile != "" || exists(t, dir) {
		t.Errorf("both clash: %+v %q", req, warns)
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
