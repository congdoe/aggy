package policyhook

import (
	"encoding/json"
	"os"
	"path/filepath"
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
		{"patch with no path", "patch", map[string]any{"patch": "*** Begin Patch"}, nil, true},
		{"search", "search_files", map[string]any{"pattern": "x", "path": "src"},
			[]parity.Call{{Kind: parity.CallSearch, Text: "/repo/src"}, {Kind: parity.CallSearch, Text: "x"}}, true},
		{"terminal mistyped", "terminal", map[string]any{"command": 1}, nil, true},
		{"terminal missing", "terminal", map[string]any{}, nil, true},
		{"other with a command", "mcp_x_run", map[string]any{"command": "x"}, []parity.Call{{Kind: parity.CallShell, Text: "x", Other: true}}, true},
		{"code", "execute_code", map[string]any{"code": "print(1)"}, []parity.Call{{Kind: parity.CallShell, Text: "print(1)", Other: true}}, true},
		{"read-only, not judged", "web_search", map[string]any{"query": "x"}, nil, false},
		{"not classified, judged", "delegate_task", map[string]any{"goal": "x"}, nil, true},
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
		if _, judged := HermesCalls([]byte(bad), "/repo"); !judged {
			t.Errorf("HermesCalls(%q) not judged", bad)
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
		// #134 review, H1: code asks even when no rule matches; anything not
		// known to be read-only asks; the read-only list has no opinion.
		{"code with no match still asks", "execute_code", map[string]any{"code": "print(1)"}, parity.VerdictAsk},
		{"browser code asks", "browser_exec", map[string]any{"code": "print(page.title())"}, parity.VerdictAsk},
		{"stdin with no match still asks", "process_manage", map[string]any{"action": "submit", "session_id": "proc_1", "data": "ls"}, parity.VerdictAsk},
		{"process kill asks", "process_manage", map[string]any{"action": "kill", "session_id": "proc_1"}, parity.VerdictAsk},
		{"process poll", "process_manage", map[string]any{"action": "poll", "session_id": "proc_1"}, parity.VerdictNone},
		{"delegate asks", "delegate_task", map[string]any{"goal": "x"}, parity.VerdictAsk},
		{"skill_manage asks", "skill_manage", map[string]any{"action": "create", "name": "x"}, parity.VerdictAsk},
		{"memory asks", "memory", map[string]any{"action": "add", "content": "x"}, parity.VerdictAsk},
		{"an MCP tool asks", "mcp_github_search", map[string]any{"query": "x"}, parity.VerdictAsk},
		{"a future tool asks", "teleport", map[string]any{}, parity.VerdictAsk},
		{"web search", "web_search", map[string]any{"query": "x"}, parity.VerdictNone},
		{"clarify", "clarify", map[string]any{"questions": []any{}}, parity.VerdictNone},
		{"terminal in another folder asks", "terminal", map[string]any{"command": "ls", "workdir": "/etc"}, parity.VerdictAsk},
		{"terminal in the repo", "terminal", map[string]any{"command": "ls", "workdir": "src", "background": true}, parity.VerdictNone},
		{"terminal workdir mistyped asks", "terminal", map[string]any{"command": "ls", "workdir": 3}, parity.VerdictAsk},
		{"a nested hermes in aggy's profile asks", "terminal", map[string]any{"command": "hermes -p aggy-0123456789ab chat -q hi"}, parity.VerdictAsk},
		{"vision on the web", "vision_analyze", map[string]any{"image_url": "https://x/y.png", "question": "?"}, parity.VerdictNone},
		{"vision on a file outside asks", "vision_analyze", map[string]any{"image_url": "/etc/y.png", "question": "?"}, parity.VerdictAsk},
		{"vision in the repo", "vision_analyze", map[string]any{"image_url": "shot.png", "question": "?"}, parity.VerdictNone},
		// H2: aggy's index, by any spelling of its folder.
		{"sqlite3 on the index asks", "terminal", map[string]any{"command": `sqlite3 ~/.aggy/aggy.db "update hermes_launches set token=''"`}, parity.VerdictAsk},
		{"the index by $HOME asks", "terminal", map[string]any{"command": "cp x ${HOME}/.aggy/aggy.db-wal"}, parity.VerdictAsk},
		{"the index by $AGGY_HOME asks", "terminal", map[string]any{"command": "cd $AGGY_HOME && sqlite3 aggy.db .dump"}, parity.VerdictAsk},
		{"the diag log asks", "terminal", map[string]any{"command": "rm diag.db"}, parity.VerdictAsk},
		{"the profile by ~ asks", "terminal", map[string]any{"command": "cat ~/.hermes/profiles/aggy-0123456789ab/config.yaml"}, parity.VerdictAsk},
	}
	p.Protected = append(p.Protected, "/home/u/.hermes/profiles/aggy-0123456789ab")
	p.Words = []string{"aggy-0123456789ab"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := JudgeHermes(p, hermesPayload(t, tt.tool, tt.in)); got.Verdict != tt.want {
				t.Fatalf("got %+v, want %s", got, tt.want)
			}
		})
	}
}

func TestHermesOutput(t *testing.T) {
	if out := HermesOutput(parity.CallDecision{Verdict: parity.VerdictNone}, "terminal"); out != nil {
		t.Errorf("none = %q", out)
	}
	if out := HermesOutput(parity.CallDecision{Verdict: parity.VerdictAllow}, "terminal"); out != nil {
		t.Errorf("allow = %q, want nothing: Hermes' own approvals decide", out)
	}
	var deny map[string]string
	if err := json.Unmarshal(HermesOutput(parity.CallDecision{Verdict: parity.VerdictDeny, Group: "destructive"}, "terminal"), &deny); err != nil || deny["action"] != "block" || deny["message"] == "" || deny["rule_key"] != "" {
		t.Errorf("deny = %v, %v", deny, err)
	}
	var a, b map[string]string
	_ = json.Unmarshal(HermesOutput(parity.CallDecision{Verdict: parity.VerdictAsk, Group: "destructive"}, "terminal"), &a)
	_ = json.Unmarshal(HermesOutput(parity.CallDecision{Verdict: parity.VerdictAsk, Group: "destructive"}, "terminal"), &b)
	if a["action"] != "approve" || !strings.HasPrefix(a["rule_key"], "aggy-") || a["rule_key"] == b["rule_key"] {
		t.Errorf("ask = %v and %v: want approve with a new rule_key each time", a, b)
	}
	if blk := string(HermesBlock("why")); blk != `{"action":"block","message":"why"}`+"\n" {
		t.Errorf("block = %q", blk)
	}
}

// TestJudgeHermesLinks (#134 re-review, MEDIUM-1): a file tool's path is
// judged by where it leads, a link in its last part included, and a write to
// a file with a second hard link asks.
func TestJudgeHermesLinks(t *testing.T) {
	h, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("HOME", h)
	repo, state, outside := filepath.Join(h, "repo"), filepath.Join(h, ".aggy"), filepath.Join(h, "outside")
	for _, d := range []string{repo, state, outside, filepath.Join(h, ".codex")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(state, "permissions.json"), filepath.Join(outside, "file"), filepath.Join(repo, "plain.md"), filepath.Join(h, ".codex", "auth.json")} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{"notes.md": "../.aggy/permissions.json", "out.md": filepath.Join(outside, "file"), "auth.md": filepath.Join(h, ".codex", "auth.json")}
	for name, to := range links {
		if err := os.Symlink(to, filepath.Join(repo, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Link(filepath.Join(outside, "file"), filepath.Join(repo, "hard.md")); err != nil {
		t.Fatal(err)
	}
	p := plan()
	p.Root, p.Protected = repo, []string{state}
	for _, c := range []struct {
		name, tool, path string
		want             parity.Verdict
	}{
		{"write to a link into aggy's state", "write_file", "notes.md", parity.VerdictAsk},
		{"patch through a link into aggy's state", "patch", "notes.md", parity.VerdictAsk},
		{"write to a link out of the repo", "write_file", "out.md", parity.VerdictAsk},
		{"read through a link out of the repo", "read_file", "out.md", parity.VerdictAsk},
		{"write to a second hard link", "write_file", "hard.md", parity.VerdictAsk},
		{"read through a link to a credential", "read_file", "auth.md", parity.VerdictDeny},
		{"a plain file", "write_file", "plain.md", parity.VerdictNone},
		{"a new file", "write_file", "new/x.md", parity.VerdictNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			if d := JudgeHermes(p, hermesPayload(t, c.tool, map[string]any{"path": c.path, "content": "c"})); d.Verdict != c.want {
				t.Fatalf("got %+v, want %s", d, c.want)
			}
		})
	}
	// Claude's write check reads the last part's link the same way.
	if !p.protectedPath(filepath.Join(repo, "notes.md")) {
		t.Error("Claude: a write through a link into aggy's state is not protected")
	}
}

// TestJudgeHermesOnDisk (#134 review): a vendor credential file is refused,
// whatever the policy says, and a path in the repo whose folder is a link out
// of it asks; the tool's name is in the message, never its input.
func TestJudgeHermesOnDisk(t *testing.T) {
	h := t.TempDir()
	t.Setenv("HOME", h)
	repo := filepath.Join(h, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h, filepath.Join(repo, "out")); err != nil {
		t.Fatal(err)
	}
	p := plan()
	p.Root = repo
	cred := filepath.Join(h, ".codex", "auth.json")
	if d := JudgeHermes(p, hermesPayload(t, "read_file", map[string]any{"path": cred})); d.Verdict != parity.VerdictDeny {
		t.Errorf("credential read: %+v", d)
	}
	if d := JudgeHermes(p, hermesPayload(t, "write_file", map[string]any{"path": "out/x", "content": "c"})); d.Verdict != parity.VerdictAsk || d.Reason != parity.ReasonOutside {
		t.Errorf("write through a link out: %+v", d)
	}
	if d := JudgeHermes(p, hermesPayload(t, "write_file", map[string]any{"path": "src/x", "content": "c"})); d.Verdict != parity.VerdictNone {
		t.Errorf("write in the repo: %+v", d)
	}
	d := JudgeHermes(p, hermesPayload(t, "execute_code", map[string]any{"code": "print('sk-secret')"}))
	out := string(HermesOutput(d, HermesTool(hermesPayload(t, "execute_code", nil))))
	if !strings.Contains(out, "execute_code") || strings.Contains(out, "sk-secret") || !strings.Contains(out, `"approve"`) {
		t.Errorf("code ask: %s", out)
	}
	if got := HermesTool(hermesPayload(t, "x\n\x1b[31mred", nil)); got != "this tool" {
		t.Errorf("tool name shown: %q", got)
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
