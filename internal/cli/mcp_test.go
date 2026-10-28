package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPWire(t *testing.T) {
	h := isolate(t)
	writeAt(t, filepath.Join(h, ".claude.json"), `{"mcpServers":{
		"fs":{"type":"stdio","command":"npx","args":["-y","fs-mcp","/Users/me/private-repo"],"env":{}},
		"lin":{"type":"sse","url":"https://mcp.linear.app/sse"},
		"web":{"type":"http","url":"https://mcp.context7.com/mcp"},
		"keyed":{"command":"srv","env":{"API_KEY":"sk-live-123"}}}}`, 0o600)
	codexCfg := "model = \"o\"\n# token = \"sk-old-secret\"\n"
	codexPath := filepath.Join(h, ".codex", "config.toml")
	writeAt(t, codexPath, codexCfg, 0o600)
	grokCfg := filepath.Join(h, ".grok", "config.toml")

	out := mustRun(t, "mcp", "wire", "--dry-run")
	for _, want := range []string{"not copied: keyed: it sets environment variables", "not copied to codex: lin: Codex has no SSE",
		"not copied: fs: stdio; name it with --server", "codex gets web (http)", "grok gets lin (sse)", "would change"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run lacks %q:\n%s", want, out)
		}
	}
	// The dry run names servers and files, never a value or a line of
	// the existing config.
	for _, leak := range []string{"sk-live-123", "sk-old-secret", "https://", "private-repo", "npx"} {
		if strings.Contains(out, leak) {
			t.Errorf("dry run shows %q:\n%s", leak, out)
		}
	}
	if _, err := os.Stat(grokCfg); !os.IsNotExist(err) {
		t.Fatal("the dry run wrote Grok's config")
	}
	mustRun(t, "mcp", "wire")
	if b, _ := os.ReadFile(codexPath); string(b) != codexCfg+"\n[mcp_servers.web]\nurl = \"https://mcp.context7.com/mcp\"\n" {
		t.Errorf("codex config = %q", b)
	}
	b, _ := os.ReadFile(grokCfg)
	if !strings.Contains(string(b), "[mcp_servers.lin]\nurl = \"https://mcp.linear.app/sse\"\ntype = \"sse\"\n") ||
		strings.Contains(string(b), "keyed") || strings.Contains(string(b), "fs") {
		t.Errorf("grok config = %q", b)
	}
	if out := mustRun(t, "mcp", "wire"); !strings.Contains(out, "Nothing to change") || !strings.Contains(out, "codex already has web") {
		t.Errorf("rewire: %s", out)
	}
	// A stdio server is copied only when named, and then only it.
	out = mustRun(t, "mcp", "wire", "--server", "fs", "--vendor", "codex")
	if !strings.Contains(out, "codex gets fs (stdio)") || strings.Contains(out, "grok") {
		t.Errorf("--server fs: %s", out)
	}
	if b, _ := os.ReadFile(codexPath); !strings.Contains(string(b), "[mcp_servers.fs]\ncommand = \"npx\"") {
		t.Errorf("codex config = %q", b)
	}
	mustRun(t, "unwire", "--all")
	if b, _ := os.ReadFile(codexPath); string(b) != codexCfg {
		t.Errorf("unwire left codex config %q", b)
	}
	if _, err := os.Stat(grokCfg); !os.IsNotExist(err) {
		t.Errorf("unwire left %s", grokCfg)
	}
	if code, _, errOut := run("mcp", "wire", "--server", "nope"); code != 1 || !strings.Contains(errOut, "no such MCP server") {
		t.Errorf("unknown --server: exit %d: %s", code, errOut)
	}
	for _, args := range [][]string{{"mcp"}, {"mcp", "wire", "x"}, {"mcp", "wire", "--vendor", "claude"}} {
		if code, _, _ := run(args...); code != 2 {
			t.Errorf("%q: exit %d", args, code)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if code, _, errOut := run("mcp", "wire"); code != 1 || !strings.Contains(errOut, "unverified") {
		t.Errorf("CLAUDE_CONFIG_DIR: exit %d: %s", code, errOut)
	}
}
