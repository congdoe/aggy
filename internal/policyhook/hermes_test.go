package policyhook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/parity"
)

// hermesPayload is a pre_tool_call payload in Hermes' shape
// (agent/shell_hooks.py:87-107 at v0.23.1): cwd is the daemon's folder.
func hermesPayload(t *testing.T, tool string, input map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"hook_event_name": "pre_tool_call", "tool_name": tool, "tool_input": input,
		"session_id": "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "cwd": "/opt/hermes", "profile": "aggy-0123456789ab",
		"extra": map[string]any{"task_id": "", "conversation": "secret words"}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHermesCalls(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		input map[string]any
		want  []parity.Call
		ok    bool
	}{
		{"terminal", "terminal", map[string]any{"command": "ls"}, []parity.Call{{Kind: parity.CallShell, Text: "ls"}}, true},
		{"read relative, from the repo root not cwd", "read_file", map[string]any{"path": ".env"}, []parity.Call{{Kind: parity.CallRead, Text: "/repo/.env"}}, true},
		{"write", "write_file", map[string]any{"path": "/x/a", "content": "c"}, []parity.Call{{Kind: parity.CallWrite, Text: "/x/a"}}, true},
		{"patch", "patch", map[string]any{"path": "a.go"}, []parity.Call{{Kind: parity.CallWrite, Text: "/repo/a.go"}}, true},
		{"patch with no path", "patch", map[string]any{"patch": "*** Begin Patch"}, nil, false},
		{"search", "search_files", map[string]any{"pattern": "x", "path": "src"},
			[]parity.Call{{Kind: parity.CallSearch, Text: "/repo/src"}, {Kind: parity.CallSearch, Text: "x"}}, true},
		{"terminal mistyped", "terminal", map[string]any{"command": 1}, nil, false},
		{"terminal missing", "terminal", map[string]any{}, nil, false},
		{"other with a command", "mcp_x_run", map[string]any{"command": "x"}, []parity.Call{{Kind: parity.CallShell, Text: "x", Other: true}}, true},
		{"other with nothing", "web_search", map[string]any{"query": "x"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := HermesCalls(hermesPayload(t, tt.tool, tt.input), "/repo")
			if ok != tt.ok || len(got) != len(tt.want) {
				t.Fatalf("got %+v, %v; want %+v, %v", got, ok, tt.want, tt.ok)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("call %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
	for _, bad := range []string{"", "not json", "[1]", `{"tool_input":{}}`} {
		if _, ok := HermesCalls([]byte(bad), "/repo"); ok {
			t.Errorf("HermesCalls(%q) ok", bad)
		}
	}
}

func TestJudgeHermes(t *testing.T) {
	p := plan()
	tests := []struct {
		name string
		tool string
		in   map[string]any
		want parity.Verdict
	}{
		{"pre-approved: Hermes decides", "terminal", map[string]any{"command": force}, parity.VerdictNone},
		{"policy match asks", "terminal", map[string]any{"command": "rm -rf build"}, parity.VerdictAsk},
		{"nothing", "terminal", map[string]any{"command": "ls"}, parity.VerdictNone},
		{"secret read asks", "read_file", map[string]any{"path": "/x/.env"}, parity.VerdictAsk},
		{"unreadable asks", "terminal", map[string]any{"command": 2}, parity.VerdictAsk},
		// The shapes seen live (T6933, 2026-11-11): Hermes' approvals did not
		// cover write_file, so the hook is the only check on them.
		{"read in the repo", "read_file", map[string]any{"path": "src/a.go"}, parity.VerdictNone},
		{"read outside the repo asks", "read_file", map[string]any{"path": "/etc/hosts"}, parity.VerdictAsk},
		{"read of ~ asks", "read_file", map[string]any{"path": "~/.ssh/id_ed25519"}, parity.VerdictAsk},
		{"read climbing out asks", "read_file", map[string]any{"path": "../other/a"}, parity.VerdictAsk},
		{"write in the repo", "write_file", map[string]any{"content": "x", "path": "src/a.go"}, parity.VerdictNone},
		{"write outside the repo asks", "write_file", map[string]any{"content": "x", "path": "/tmp/x"}, parity.VerdictAsk},
		{"write to aggy's state asks", "write_file", map[string]any{"content": "x", "path": "/home/u/.aggy/aggy.db"}, parity.VerdictAsk},
		{"write to the profile asks", "write_file", map[string]any{"content": "x", "path": "/home/u/.hermes/profiles/aggy-0123456789ab/config.yaml"}, parity.VerdictAsk},
		{"write to a Claude settings file asks", "write_file", map[string]any{"content": "x", "path": ".claude/settings.json"}, parity.VerdictAsk},
		{"search in the repo", "search_files", map[string]any{"path": "src", "pattern": "/api/"}, parity.VerdictNone},
		{"search outside the repo asks", "search_files", map[string]any{"path": "/home/u", "pattern": "x"}, parity.VerdictAsk},
		{"search with no path", "search_files", map[string]any{"pattern": "TODO"}, parity.VerdictNone},
		{"cron asks", "cronjob_manage", map[string]any{"action": "list"}, parity.VerdictAsk},
		{"code that pushes asks", "execute_code", map[string]any{"code": "import os; os.system('git push --force')"}, parity.VerdictAsk},
		{"stdin typed into a shell is judged", "process_manage", map[string]any{"action": "submit", "session_id": "proc_1", "data": "rm -rf build"}, parity.VerdictAsk},
		{"process list", "process_manage", map[string]any{"action": "list"}, parity.VerdictNone},
	}
	p.Protected = append(p.Protected, "/home/u/.hermes/profiles/aggy-0123456789ab")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := JudgeHermes(p, hermesPayload(t, tt.tool, tt.in)); got.Verdict != tt.want {
				t.Fatalf("got %+v, want %s", got, tt.want)
			}
		})
	}
}

func TestHermesOutput(t *testing.T) {
	if out := HermesOutput(parity.CallDecision{Verdict: parity.VerdictNone}); out != nil {
		t.Errorf("none = %q", out)
	}
	if out := HermesOutput(parity.CallDecision{Verdict: parity.VerdictAllow}); out != nil {
		t.Errorf("allow = %q, want nothing: Hermes' own approvals decide", out)
	}
	var deny map[string]string
	if err := json.Unmarshal(HermesOutput(parity.CallDecision{Verdict: parity.VerdictDeny, Group: "destructive"}), &deny); err != nil || deny["action"] != "block" || deny["message"] == "" || deny["rule_key"] != "" {
		t.Errorf("deny = %v, %v", deny, err)
	}
	var a, b map[string]string
	_ = json.Unmarshal(HermesOutput(parity.CallDecision{Verdict: parity.VerdictAsk, Group: "destructive"}), &a)
	_ = json.Unmarshal(HermesOutput(parity.CallDecision{Verdict: parity.VerdictAsk, Group: "destructive"}), &b)
	if a["action"] != "approve" || !strings.HasPrefix(a["rule_key"], "aggy-") || a["rule_key"] == b["rule_key"] {
		t.Errorf("ask = %v and %v: want approve with a new rule_key each time", a, b)
	}
	if blk := string(HermesBlock("why")); blk != `{"action":"block","message":"why"}`+"\n" {
		t.Errorf("block = %q", blk)
	}
}

// TestParseHermesNeverKeepsExtra: the payload's extra and cwd are not read.
func TestParseHermesNeverKeepsExtra(t *testing.T) {
	p, ok := ParseHermes(hermesPayload(t, "terminal", map[string]any{"command": "ls"}))
	if !ok || p.Profile != "aggy-0123456789ab" || p.SessionID != "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" || p.Event != "pre_tool_call" {
		t.Fatalf("got %+v", p)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "secret words") || strings.Contains(string(b), "/opt/hermes") {
		t.Errorf("kept %s", b)
	}
}
