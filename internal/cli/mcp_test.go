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
		"fs":{"type":"stdio","command":"npx","args":["-y","fs-mcp"],"env":{}},
		"lin":{"type":"sse","url":"https://mcp.linear.app/sse"},
		"keyed":{"command":"srv","env":{"API_KEY":"sk-live-123"}}}}`, 0o600)
	codexCfg := "model = \"o\"\n"
	writeAt(t, filepath.Join(h, ".codex", "config.toml"), codexCfg, 0o600)
	grokCfg := filepath.Join(h, ".grok", "config.toml")

	out := mustRun(t, "mcp", "wire", "--dry-run")
	for _, want := range []string{"not copied: keyed: it sets environment variables", "not copied to codex: lin: Codex has no SSE",
		"codex gets fs", "grok gets lin", "+[mcp_servers.fs]"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "sk-live-123") {
		t.Error("a server's env value was printed")
	}
	if _, err := os.Stat(grokCfg); !os.IsNotExist(err) {
		t.Fatal("the dry run wrote Grok's config")
	}
	mustRun(t, "mcp", "wire")
	b, _ := os.ReadFile(filepath.Join(h, ".codex", "config.toml"))
	if string(b) != codexCfg+"\n[mcp_servers.fs]\ncommand = \"npx\"\nargs = [\"-y\", \"fs-mcp\"]\n" {
		t.Errorf("codex config = %q", b)
	}
	b, _ = os.ReadFile(grokCfg)
	if !strings.Contains(string(b), "[mcp_servers.lin]\nurl = \"https://mcp.linear.app/sse\"\ntype = \"sse\"\n") || strings.Contains(string(b), "keyed") {
		t.Errorf("grok config = %q", b)
	}
	if out := mustRun(t, "mcp", "wire"); !strings.Contains(out, "Nothing to change") || !strings.Contains(out, "codex already has fs") {
		t.Errorf("rewire: %s", out)
	}
	mustRun(t, "unwire", "--all")
	if b, _ := os.ReadFile(filepath.Join(h, ".codex", "config.toml")); string(b) != codexCfg {
		t.Errorf("unwire left codex config %q", b)
	}
	if _, err := os.Stat(grokCfg); !os.IsNotExist(err) {
		t.Errorf("unwire left %s", grokCfg)
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
