package toolwire

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/switchboard"
	"github.com/congdoe/aggy/internal/tooldef"
)

func onState(name string, argv ...string) switchboard.State {
	m := &tooldef.MCP{Vendors: []adapter.Vendor{adapter.Claude}, Argv: argv, Env: []string{"DEMO_HOME"}}
	return switchboard.State{Tool: tooldef.Tool{Def: tooldef.Def{Name: name}}, Vendor: adapter.Claude, On: true,
		Parts: tooldef.Parts{MCP: m}}
}

func TestMCP(t *testing.T) {
	on := []switchboard.State{
		onState("idx", "idx", "serve", "--path", "{repo}"),
		onState("docs", "docs-mcp"),
		onState("mine", "mine-mcp"),
		{Tool: tooldef.Tool{Def: tooldef.Def{Name: "skillonly"}}, On: true}, // no MCP part
	}
	got, refused := MCP(on, "/r/app", "/state/run/x", []string{"mine"})
	want := []adapter.MCPServer{
		{Name: "idx", Command: "idx", Args: []string{"serve", "--path", "/r/app"}, EnvVars: []string{"DEMO_HOME"}, Cwd: "/state/run/x"},
		{Name: "docs", Command: "docs-mcp", Args: []string{}, EnvVars: []string{"DEMO_HOME"}, Cwd: "/state/run/x"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("servers = %+v\nwant %+v", got, want)
	}
	if len(refused) != 1 || refused[0].Tool != "mine" || refused[0].Code != diag.CodeToolNameClash {
		t.Errorf("refused = %+v, want mine's name clash", refused)
	}
	// F-3: {repo} with no repo, or a path outside CheckBin's set.
	for _, repo := range []string{"", "/r/my app", "/r/a\"b", "/r/a$(x)", "/r/a\nb", "/r/é", "r/rel"} {
		got, refused := MCP(on[:1], repo, "/d", nil)
		if len(got) != 0 || len(refused) != 1 || refused[0].Code != diag.CodeToolRepoPath {
			t.Errorf("repo %q: %+v %+v", repo, got, refused)
		}
	}
	// A tool without {repo} is wired outside a repo.
	if got, refused := MCP(on[1:2], "", "/d", nil); len(got) != 1 || len(refused) != 0 {
		t.Errorf("outside a repo: %+v %+v", got, refused)
	}
	if got, refused := MCP(nil, "/r/app", "/d", nil); got != nil || refused != nil {
		t.Errorf("nothing on: %+v %+v", got, refused)
	}
}

func TestClaudeConfig(t *testing.T) {
	b, err := ClaudeConfig([]adapter.MCPServer{{Name: "idx", Command: "idx", Args: []string{"--a", `x"y`}, Cwd: "/d"}, {Name: "docs", Command: "docs-mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.MCPServers) != 2 || doc.MCPServers["idx"]["command"] != "idx" || doc.MCPServers["docs"]["args"] == nil {
		t.Errorf("config = %s", b)
	}
	if _, ok := doc.MCPServers["idx"]["cwd"]; ok {
		t.Error("Claude's entry has no cwd field")
	}
}

// TestGrokAgent: the frontmatter keeps its shape whatever an argument
// holds: each server is one line of JSON, which YAML reads as a flow map.
func TestGrokAgent(t *testing.T) {
	odd := []string{"a: b", "- x", "---", "x\ny", `"q"`, "#c", "{repo}", " "}
	b, err := GrokAgent([]adapter.MCPServer{{Name: "idx", Command: "idx", Args: odd, Cwd: "/state/d"}, {Name: "docs", Command: "docs-mcp"}})
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	head := []string{"---", "name: aggy-tools", `description: "aggy: the managed tools turned on for this session"`, "mcpServers:"}
	if !reflect.DeepEqual(lines[:4], head) || lines[6] != "---" || strings.Count(string(b), "\n---\n") != 1 {
		t.Fatalf("agent file:\n%s", b)
	}
	var e map[string]struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Cwd     string   `json:"cwd"`
	}
	entry, ok := strings.CutPrefix(lines[4], "  - ")
	if !ok || json.Unmarshal([]byte(entry), &e) != nil || !reflect.DeepEqual(e["idx"].Args, odd) || e["idx"].Cwd != "/state/d" {
		t.Errorf("entry %q", lines[4])
	}
}
