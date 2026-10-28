package parity

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/patch"
)

func TestClaudeServer(t *testing.T) {
	cases := []struct {
		name, entry string
		want        MCPServer
		why         string
	}{
		{"fs", `{"type":"stdio","command":"npx","args":["-y","@mcp/fs","/tmp"],"env":{}}`,
			MCPServer{Name: "fs", Type: "stdio", Command: "npx", Args: []string{"-y", "@mcp/fs", "/tmp"}}, ""},
		{"bare", `{"command":"srv"}`, MCPServer{Name: "bare", Type: "stdio", Command: "srv"}, ""},
		{"web", `{"type":"http","url":"https://mcp.example.com/mcp","headers":{}}`,
			MCPServer{Name: "web", Type: "http", URL: "https://mcp.example.com/mcp"}, ""},
		{"alias", `{"type":"streamable-http","url":"http://localhost:8080/mcp"}`,
			MCPServer{Name: "alias", Type: "http", URL: "http://localhost:8080/mcp"}, ""},
		{"lin", `{"type":"sse","url":"https://mcp.linear.app/sse"}`, MCPServer{Name: "lin", Type: "sse", URL: "https://mcp.linear.app/sse"}, ""},
		{"env", `{"command":"x","env":{"API_KEY":"v"}}`, MCPServer{}, "environment"},
		{"hdr", `{"type":"http","url":"https://a.example/mcp","headers":{"Authorization":"Bearer x"}}`, MCPServer{}, "headers"},
		{"helper", `{"type":"http","url":"https://a.example/mcp","headersHelper":"get-token"}`, MCPServer{}, "headers"},
		{"oauth", `{"type":"http","url":"https://a.example/mcp","oauth":{"clientId":"c"}}`, MCPServer{}, "OAuth"},
		{"argtok", `{"command":"srv","args":["--token","abc"]}`, MCPServer{}, "secret"},
		{"argkey", `{"command":"srv","args":["--api-key=abc"]}`, MCPServer{}, "secret"},
		{"secretval", `{"command":"srv","args":["ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]}`, MCPServer{}, "secret"},
		{"expand", `{"command":"${HOME}/bin/srv"}`, MCPServer{}, "${VAR}"},
		{"ctrl", `{"command":"srv\u001b[31m"}`, MCPServer{}, "control"},
		{"query", `{"type":"http","url":"https://a.example/mcp?key=abc"}`, MCPServer{}, "query"},
		{"userinfo", `{"type":"http","url":"https://bob@a.example/mcp"}`, MCPServer{}, "user info"},
		{"composio", `{"type":"http","url":"https://mcp.composio.dev/composio/server/mcp/6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f"}`, MCPServer{}, "looks like a key"},
		{"capurl", `{"type":"http","url":"https://mcp.example.com/u/AbCdEf0123456789XyZ/mcp"}`, MCPServer{}, "looks like a key"},
		{"hexkey", `{"type":"http","url":"https://mcp.example.com/s/9f86d081884c7d659a2feaa0c55ad015/mcp"}`, MCPServer{}, "looks like a key"},
		{"context7", `{"type":"http","url":"https://mcp.context7.com/mcp"}`, MCPServer{Name: "context7", Type: "http", URL: "https://mcp.context7.com/mcp"}, ""},
		{"longword", `{"type":"http","url":"https://api.example.com/modelcontextprotocol/streamable-http/mcp"}`,
			MCPServer{Name: "longword", Type: "http", URL: "https://api.example.com/modelcontextprotocol/streamable-http/mcp"}, ""},
		{"plainhttp", `{"type":"http","url":"http://a.example/mcp"}`, MCPServer{}, "https"},
		{"ws", `{"type":"ws","url":"wss://a.example/mcp"}`, MCPServer{}, "transport"},
		{"mixed", `{"type":"http","url":"https://a.example/mcp","command":"x"}`, MCPServer{}, "no command"},
		{"bad name!", `{"command":"x"}`, MCPServer{}, "name"},
		{"notjson", `[1]`, MCPServer{}, "not understood"},
	}
	// G2 re-review: a key in a host label, a short key, and a key split
	// across short segments are refused; common public servers are not.
	for i, u := range []string{
		"https://k7xp2qwz9a8b4c6d1e3f.mcp.example.com/mcp",
		"https://mcp.example.com/mcp/k7Xp2QwZ9a",
		"https://mcp.example.com/ab12/CD34/ef56/GH78",
		"https://6f1c2d3e-4a5b-4c6d-8e9f-0a1b2c3d4e5f.mcp.example.com/mcp",
		"https://qwhzkrtplmnbvcxzjgfd.mcp.example.com/mcp",
		// I43-7: all digits, lower case with repeats, and short labels.
		"https://mcp.example.com/s/12345678901234567890/mcp",
		"https://12345678901234567890.mcp.example.com/mcp",
		"https://mcp.example.com/s/kqzvtwmrjxhplbnaqzvt",
		"https://abcd1234efgh.mcp.example.com/mcp",
		"https://kqzvtwmrjxhplbn.mcp.example.com/mcp",
		// I43-11: 15 digits in a host name, and short base64.
		"https://123456789012345.mcp.example.com/mcp",
		"https://mcp.example.com/s/QUJDREVGR0g/mcp",
	} {
		cases = append(cases, struct {
			name, entry string
			want        MCPServer
			why         string
		}{
			fmt.Sprintf("leak%d", i), `{"type":"http","url":"` + u + `"}`, MCPServer{}, "looks like a key"})
	}
	for i, u := range []string{
		"https://mcp.linear.app/sse",
		"https://mcp.linear.app/mcp",
		"https://mcp.notion.com/mcp",
		"https://api.githubcopilot.com/mcp/",
		"https://api.githubcopilot.com/mcp/x/repos/readonly",
		"https://mcp.sentry.dev/mcp",
		"https://mcp.atlassian.com/v1/sse",
		"https://mcp.stripe.com",
		"https://mcp.supabase.com/mcp",
		"https://mcp.context7.com/mcp",
		"https://docs.mcp.cloudflare.com/sse",
		"https://docs.mcp.cloudflare.com/mcp",
		"https://mcp.deepwiki.com/mcp",
		"http://127.0.0.1:3845/mcp",
		"http://127.0.0.1:3845/sse",
		"https://evalstate-flux1-schnell.hf.space/gradio_api/mcp/sse",
		"https://huggingface.co/mcp",
		"https://gitmcp.io/microsoft/TypeScript",
		"https://gitmcp.io/modelcontextprotocol/typescript-sdk",
		"https://gitmcp.io/congdoe/aggy",
		"https://gitmcp.io/docs",
		// N55-6: acronyms with a digit, and AWS account and GCP project
		// numbers in a path.
		"https://mcp.example.com/OPENAPI3/mcp",
		"https://mcp.example.com/mcp/GITHUB2",
		"https://mcp.example.com/aws/123456789012/mcp",
		"https://mcp.example.com/projects/123456789012/mcp",
	} {
		name := fmt.Sprintf("public%d", i)
		cases = append(cases, struct {
			name, entry string
			want        MCPServer
			why         string
		}{
			name, `{"type":"http","url":"` + u + `"}`, MCPServer{Name: name, Type: "http", URL: u}, ""})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, why := claudeServer(tc.name, []byte(tc.entry))
			if tc.why == "" {
				if why != "" || !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("got %+v, %q; want %+v", got, why, tc.want)
				}
				return
			}
			if !strings.Contains(why, tc.why) {
				t.Fatalf("why = %q, want it to mention %q", why, tc.why)
			}
		})
	}
}

func TestReadClaudeMCP(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	if s, k, err := ReadClaudeMCP(path); err != nil || s != nil || k != nil {
		t.Fatalf("missing file: %v %v %v", s, k, err)
	}
	body := `{"oauthAccount":{"emailAddress":"x"},"mcpServers":{"zeta":{"command":"z"},"alpha":{"command":"a","env":{"K":"v"}},"a b":{"command":"x","env":{"K":"v"}}},
	"projects":{"/r":{"mcpServers":{"local":{"command":"l"}}}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	s, k, err := ReadClaudeMCP(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s) != 1 || s[0].Name != "zeta" {
		t.Errorf("servers = %+v, want only zeta (user scope, no local ones)", s)
	}
	if len(k) != 2 || k[0].Name != "(a server with an unusual name)" || k[1].Name != "alpha" {
		t.Errorf("skips = %+v", k)
	}
}

func TestClaudeJSONPath(t *testing.T) {
	env := map[string]string{"HOME": "/h"}
	if p, err := ClaudeJSONPath(func(k string) string { return env[k] }); err != nil || p != "/h/.claude.json" {
		t.Errorf("got %q, %v", p, err)
	}
	env["CLAUDE_CONFIG_DIR"] = "/c"
	if _, err := ClaudeJSONPath(func(k string) string { return env[k] }); err == nil {
		t.Error("CLAUDE_CONFIG_DIR: want an error, the location is unverified")
	}
}

var (
	fsServer  = MCPServer{Name: "fs", Type: "stdio", Command: `C:\bin\"srv"`, Args: []string{"-y", "pkg"}}
	webServer = MCPServer{Name: "web", Type: "http", URL: "https://mcp.example.com/mcp"}
	sseServer = MCPServer{Name: "lin", Type: "sse", URL: "https://mcp.linear.app/sse"}
)

func TestAddServers(t *testing.T) {
	cases := []struct {
		name string
		v    adapter.Vendor
		in   []byte
		want string
	}{
		{"missing file", adapter.Codex, nil,
			"[mcp_servers.fs]\ncommand = \"C:\\\\bin\\\\\\\"srv\\\"\"\nargs = [\"-y\", \"pkg\"]\n\n[mcp_servers.web]\nurl = \"https://mcp.example.com/mcp\"\n"},
		{"crlf no trailing newline", adapter.Codex, []byte("# mine\r\nmodel = \"o\"\r\n[tui]\r\nx = 1"),
			"# mine\r\nmodel = \"o\"\r\n[tui]\r\nx = 1\r\n\r\n[mcp_servers.fs]\r\ncommand = \"C:\\\\bin\\\\\\\"srv\\\"\"\r\nargs = [\"-y\", \"pkg\"]\r\n\r\n[mcp_servers.web]\r\nurl = \"https://mcp.example.com/mcp\"\r\n"},
		{"existing kept", adapter.Grok, []byte("[mcp_servers.fs]\ncommand = \"mine\"\n"),
			"[mcp_servers.fs]\ncommand = \"mine\"\n\n[mcp_servers.web]\nurl = \"https://mcp.example.com/mcp\"\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := addServers(tc.v, tc.in, []MCPServer{fsServer, webServer})
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tc.want {
				t.Fatalf("got\n%q\nwant\n%q", out, tc.want)
			}
			again, err := addServers(tc.v, out, []MCPServer{fsServer, webServer})
			if err != nil || !bytes.Equal(again, out) {
				t.Errorf("not idempotent: %q, %v", again, err)
			}
		})
	}
	out, err := addServers(adapter.Grok, nil, []MCPServer{sseServer})
	if err != nil || string(out) != "[mcp_servers.lin]\nurl = \"https://mcp.linear.app/sse\"\ntype = \"sse\"\n" {
		t.Errorf("grok sse: %q, %v", out, err)
	}
	for _, in := range []string{
		"mcp_servers = { other = { command = \"x\" } }\n", // inline table: cannot be extended
		"mcp_servers = 1\n",
		"model = \n",
	} {
		if out, err := addServers(adapter.Codex, []byte(in), []MCPServer{webServer}); err == nil {
			t.Errorf("addServers(%q) = %q, want an error", in, out)
		}
	}
}

// TestPlanMCPWire: existing names are kept, a transport the vendor lacks
// is skipped, and the patch applies and reverts byte for byte.
func TestPlanMCPWire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	orig := []byte("model = \"o\"\n\n[mcp_servers.web]\nurl = \"https://mine.example/mcp\"\n")
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := PlanMCPWire(adapter.Codex, path, []MCPServer{fsServer, webServer, sseServer})
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Add) != 1 || w.Add[0].Name != "fs" || !reflect.DeepEqual(w.Have, []string{"web"}) ||
		len(w.Skip) != 1 || w.Skip[0].Name != "lin" {
		t.Fatalf("plan = %+v", w)
	}
	rec, err := patch.Apply(w.Patch, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if !bytes.Contains(b, []byte("[mcp_servers.fs]")) || !bytes.HasPrefix(b, orig) {
		t.Fatalf("after apply: %q", b)
	}
	if err := patch.Revert(rec); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !bytes.Equal(b, orig) {
		t.Fatalf("after revert: %q", b)
	}
	w, err = PlanMCPWire(adapter.Codex, path, []MCPServer{webServer})
	if err != nil || len(w.Add) != 0 || w.Patch.File != "" {
		t.Errorf("nothing to add: %+v, %v", w, err)
	}
	// Grok has SSE, but a url ending in /sse is SSE to it whatever the type.
	w, err = PlanMCPWire(adapter.Grok, filepath.Join(dir, "none.toml"),
		[]MCPServer{sseServer, {Name: "odd", Type: "http", URL: "https://a.example/sse"}})
	if err != nil || len(w.Add) != 1 || len(w.Skip) != 1 || w.Skip[0].Name != "odd" {
		t.Errorf("grok plan = %+v, %v", w, err)
	}
}

func TestMCPNames(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"HOME": home}
	get := func(k string) string { return env[k] }
	write := func(p, s string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(home, ".claude.json"), `{"mcpServers":{"a":{},"b":{}},"projects":{"/r":{"mcpServers":{"loc":{}}},"/other":{"mcpServers":{"no":{}}}}}`)
	write(filepath.Join(home, ".codex", "config.toml"), "[mcp_servers.a]\ncommand = \"x\"\n[mcp_servers.off]\ncommand = \"x\"\nenabled = false\n")
	write(filepath.Join(home, ".grok", "config.toml"), "[mcp_servers.g]\ncommand = \"x\"\n")
	check := func(v adapter.Vendor, want ...string) {
		t.Helper()
		got, err := MCPNames(v, "/r", get)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v, %v; want %v", v, got, err, want)
		}
	}
	check(adapter.Claude, "a", "b", "loc")
	check(adapter.Codex, "a")
	check(adapter.Grok, "a", "b", "g", "loc")
	env["GROK_CLAUDE_MCPS_ENABLED"] = "0"
	check(adapter.Grok, "g")
	delete(env, "GROK_CLAUDE_MCPS_ENABLED")
	write(filepath.Join(home, ".grok", "config.toml"), "[compat.claude]\nmcps = false\n")
	check(adapter.Grok)
}

// TestProjectMCPNames: the servers a repo's own config gives each CLI,
// for name clashes (T6708); a file that cannot be parsed is an error.
func TestProjectMCPNames(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	sub := filepath.Join(root, "a", "b")
	w := func(p, s string) {
		t.Helper()
		must(t, os.MkdirAll(filepath.Dir(p), 0o700))
		must(t, os.WriteFile(p, []byte(s), 0o600))
	}
	w(filepath.Join(root, ".mcp.json"), `{"mcpServers":{"top":{"command":"x"}}}`)
	w(filepath.Join(sub, ".mcp.json"), `{"mcpServers":{"low":{"command":"x"}}}`)
	w(filepath.Join(root, "a", ".grok", "config.toml"), "[mcp_servers.mid]\ncommand = \"x\"\n")
	w(filepath.Join(root, "a", ".codex", "config.toml"), "[mcp_servers.cx]\ncommand = \"x\"\n")
	for _, c := range []struct {
		v    adapter.Vendor
		want string
	}{
		{adapter.Claude, "top"}, {adapter.Codex, "cx"}, {adapter.Grok, "low mid top"},
	} {
		got, err := ProjectMCPNames(c.v, sub, root)
		slices.Sort(got)
		if err != nil || strings.Join(got, " ") != c.want {
			t.Errorf("%s: %q %v, want %q", c.v, got, err, c.want)
		}
	}
	w(filepath.Join(root, "a", ".codex", "config.toml"), "[mcp_servers\n")
	w(filepath.Join(sub, ".mcp.json"), `{`)
	for _, v := range []adapter.Vendor{adapter.Codex, adapter.Grok} {
		if _, err := ProjectMCPNames(v, sub, root); err == nil {
			t.Errorf("%s: a broken file read as none", v)
		}
	}
}
