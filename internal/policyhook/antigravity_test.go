package policyhook

import (
	"encoding/json"
	"testing"

	"github.com/congdoe/aggy/internal/parity"
)

// agyPayload is a PreToolUse payload in agy's shape (agy 1.5.5, live): the
// tool call under toolCall, with keys the hook never reads.
func agyPayload(t *testing.T, tool string, args map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"hookEventName": "PreToolUse", "conversationId": "11111111-2222-4333-8444-555555555555",
		"lastUserInput": "secret words", "toolCall": map[string]any{"id": "t1", "name": tool, "args": args}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAntigravityCalls(t *testing.T) {
	tests := []struct {
		name string
		tool string
		args map[string]any
		want []parity.Call
		ok   bool
	}{
		{"shell", "run_command", map[string]any{"CommandLine": "ls", "Cwd": "/repo", "WaitMsBeforeAsync": 0}, []parity.Call{{Kind: parity.CallShell, Text: "ls"}}, true},
		{"shell missing", "run_command", map[string]any{}, nil, false},
		{"shell mistyped", "run_command", map[string]any{"CommandLine": 1}, nil, false},
		{"view", "view_file", map[string]any{"AbsolutePath": "/repo/.env"}, []parity.Call{{Kind: parity.CallRead, Text: "/repo/.env"}}, true},
		// #141 review, HIGH-1: a known tool missing its expected key is judged
		// by every path-like string, and asks when it has none.
		{"read_file with no path-like arg asks", "read_file", map[string]any{"Path": "x"}, nil, false},
		// #144 re-review, MEDIUM-A: a known read or search tool missing its
		// key asks (ok false), its strings still judged.
		{"read_file by another key", "read_file", map[string]any{"Path": "/home/u/.ssh/id_rsa"}, []parity.Call{{Kind: parity.CallRead, Text: "/home/u/.ssh/id_rsa"}}, false},
		{"read_file nested", "read_file", map[string]any{"Files": []any{map[string]any{"FilePath": "/repo/.env"}}},
			[]parity.Call{{Kind: parity.CallRead, Text: "/repo/.env"}, {Kind: parity.CallRead, Text: "/repo/.env/FilePath"}}, false},
		{"read_file by URI", "read_file", map[string]any{"Uri": "file:///home/u/.ssh/id_rsa", "Cwd": "/repo"},
			[]parity.Call{{Kind: parity.CallRead, Text: "/repo"}, {Kind: parity.CallRead, Text: "/home/u/.ssh/id_rsa"}}, false},
		{"read_file by name and folder", "read_file", map[string]any{"File": "id_rsa", "Dir": "/repo"},
			[]parity.Call{{Kind: parity.CallRead, Text: "/repo"}, {Kind: parity.CallRead, Text: "/repo/id_rsa"}}, false},
		{"unknown by file URI", "read_url", map[string]any{"Url": "file:///home/u/.ssh/id_rsa"},
			[]parity.Call{{Kind: parity.CallRead, Text: "/home/u/.ssh/id_rsa", Other: true}, {Kind: parity.CallWrite, Text: "/home/u/.ssh/id_rsa", Other: true}}, true},
		{"unknown by file URI with a host", "open_resource", map[string]any{"Uri": "file://localhost/repo/.env"},
			[]parity.Call{{Kind: parity.CallRead, Text: "/repo/.env", Other: true}, {Kind: parity.CallWrite, Text: "/repo/.env", Other: true}}, true},
		{"unknown with a Windows path asks", "open_resource", map[string]any{"Path": "C:\\Users\\u\\.ssh\\id_rsa"}, nil, false},
		{"unknown with a path as a key", "batch", map[string]any{"Files": map[string]any{"/home/u/.ssh/id_rsa": "read"}},
			[]parity.Call{{Kind: parity.CallRead, Text: "/home/u/.ssh/id_rsa", Other: true}, {Kind: parity.CallWrite, Text: "/home/u/.ssh/id_rsa", Other: true},
				{Kind: parity.CallRead, Text: "/home/u/.ssh/id_rsa/read", Other: true}, {Kind: parity.CallWrite, Text: "/home/u/.ssh/id_rsa/read", Other: true}}, true},
		{"read_file with nothing asks", "read_file", map[string]any{}, nil, false},
		{"search by another key", "grep_search", map[string]any{"Dir": "/etc", "Query": "x"}, []parity.Call{{Kind: parity.CallSearch, Text: "/etc"}, {Kind: parity.CallSearch, Text: "/etc/x"}}, false},
		{"search with no path asks", "list_dir", map[string]any{"Recursive": true}, nil, false},
		{"unknown with a path", "write_file", map[string]any{"Path": "/repo/.agents/hooks.json"},
			[]parity.Call{{Kind: parity.CallRead, Text: "/repo/.agents/hooks.json", Other: true}, {Kind: parity.CallWrite, Text: "/repo/.agents/hooks.json", Other: true}}, true},
		{"unknown with a URL only", "read_url", map[string]any{"Url": "https://x/y"}, nil, true},
		{"write", "write_to_file", map[string]any{"TargetFile": "/repo/a.go"}, []parity.Call{{Kind: parity.CallWrite, Text: "/repo/a.go"}}, true},
		{"replace", "replace_file_content", map[string]any{"TargetFile": "/repo/a.go"}, []parity.Call{{Kind: parity.CallWrite, Text: "/repo/a.go"}}, true},
		{"multi replace", "multi_replace_file_content", map[string]any{"TargetFile": "/repo/a.go"}, []parity.Call{{Kind: parity.CallWrite, Text: "/repo/a.go"}}, true},
		{"write missing asks", "write_to_file", map[string]any{"Content": "x"}, nil, false},
		{"grep", "grep_search", map[string]any{"SearchPath": "/repo/src"}, []parity.Call{{Kind: parity.CallSearch, Text: "/repo/src"}}, true},
		{"find", "find_by_name", map[string]any{"SearchDirectory": "/etc"}, []parity.Call{{Kind: parity.CallSearch, Text: "/etc"}}, true},
		{"list", "list_dir", map[string]any{"DirectoryPath": "/repo"}, []parity.Call{{Kind: parity.CallSearch, Text: "/repo"}}, true},
		{"send input", "manage_task", map[string]any{"Action": "send_input", "Input": "rm -rf /"}, []parity.Call{{Kind: parity.CallShell, Text: "rm -rf /", Other: true}}, true},
		{"task status", "manage_task", map[string]any{"Action": "status"}, nil, true},
		{"unknown with a command", "mcp_run", map[string]any{"CommandLine": "x"}, []parity.Call{{Kind: parity.CallShell, Text: "x", Other: true}}, true},
		{"unknown with a file", "edit_notebook", map[string]any{"TargetFile": "/x"}, []parity.Call{{Kind: parity.CallWrite, Text: "/x", Other: true}}, true},
		{"unknown, nothing known", "browser_open", map[string]any{"Url": "https://x"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := AntigravityCalls(agyPayload(t, tt.tool, tt.args))
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
	for _, bad := range []string{"", "not json", "[1]", "null", `{"toolCall":{}}`, `{"toolCall":null}`} {
		if _, ok := AntigravityCalls([]byte(bad)); ok {
			t.Errorf("AntigravityCalls(%q) read", bad)
		}
	}
}

func TestJudgeAntigravity(t *testing.T) {
	p := plan()
	p.Protected = append(p.Protected, "/repo/.agents", "/home/u/.gemini")
	p.Words = []string{".agents", "hooks.json", ".gemini"}
	tests := []struct {
		name string
		tool string
		args map[string]any
		want parity.Verdict
	}{
		{"nothing", "run_command", map[string]any{"CommandLine": "ls"}, parity.VerdictNone},
		{"policy match asks", "run_command", map[string]any{"CommandLine": "rm -rf build"}, parity.VerdictAsk},
		{"unreadable asks", "run_command", map[string]any{"CommandLine": 2}, parity.VerdictAsk},
		{"secret read asks", "view_file", map[string]any{"AbsolutePath": "/repo/.env"}, parity.VerdictAsk},
		{"read in the repo", "view_file", map[string]any{"AbsolutePath": "/repo/a.go"}, parity.VerdictNone},
		{"write in the repo", "write_to_file", map[string]any{"TargetFile": "/repo/a.go"}, parity.VerdictNone},
		{"write outside asks", "write_to_file", map[string]any{"TargetFile": "/tmp/x"}, parity.VerdictAsk},
		{"write to the hooks file asks", "write_to_file", map[string]any{"TargetFile": "/repo/.agents/hooks.json"}, parity.VerdictAsk},
		{"shell on the hooks file asks", "run_command", map[string]any{"CommandLine": "rm .agents/hooks.json"}, parity.VerdictAsk},
		{"shell on agy's folder asks", "run_command", map[string]any{"CommandLine": "cat ~/.gemini/x"}, parity.VerdictAsk},
		{"write to aggy's state asks", "replace_file_content", map[string]any{"TargetFile": "/home/u/.aggy/aggy.db"}, parity.VerdictAsk},
		{"search outside asks", "grep_search", map[string]any{"SearchPath": "/home/u"}, parity.VerdictAsk},
		{"typed input asks", "manage_task", map[string]any{"Action": "send_input", "Input": "git push --force"}, parity.VerdictAsk},
		{"relative read asks", "view_file", map[string]any{"AbsolutePath": "a.go"}, parity.VerdictAsk},
		{"climbing out asks", "write_to_file", map[string]any{"TargetFile": "/repo/../etc/x"}, parity.VerdictAsk},
		{"search in the repo", "find_by_name", map[string]any{"SearchDirectory": "/repo/src"}, parity.VerdictNone},
		{"unknown tool, no opinion", "browser_open", map[string]any{"Url": "https://x"}, parity.VerdictNone},
		// #141 review, M3: spellings that reach the hooks file ask; ordinary
		// globs and variables do not.
		{"glob to the hooks file asks", "run_command", map[string]any{"CommandLine": "rm .ag*/hooks.json"}, parity.VerdictAsk},
		{"find by name asks", "run_command", map[string]any{"CommandLine": "find . -name hooks.json -delete"}, parity.VerdictAsk},
		{"built name asks", "run_command", map[string]any{"CommandLine": "d=.ag; rm -f ${d}ents/hooks.json"}, parity.VerdictAsk},
		{"glob of the folder asks", "run_command", map[string]any{"CommandLine": "rm -rf .ag*"}, parity.VerdictAsk},
		{"question-mark glob asks", "run_command", map[string]any{"CommandLine": "mv .a?ents /tmp/x"}, parity.VerdictAsk},
		{"variable piece asks", "run_command", map[string]any{"CommandLine": "rm -r ${D}gents"}, parity.VerdictAsk},
		{"dot-star asks", "run_command", map[string]any{"CommandLine": "rm -rf .*"}, parity.VerdictAsk},
		{"plain star", "run_command", map[string]any{"CommandLine": "ls *"}, parity.VerdictNone},
		{"glob in src", "run_command", map[string]any{"CommandLine": "rm src/*.go"}, parity.VerdictNone},
		{"dot glob elsewhere", "run_command", map[string]any{"CommandLine": "ls .git*"}, parity.VerdictNone},
		{"a variable", "run_command", map[string]any{"CommandLine": "echo $HOME/x ${PATH}"}, parity.VerdictNone},
		{"agent word", "run_command", map[string]any{"CommandLine": "go test ./internal/agents/..."}, parity.VerdictNone},
		// #141 review, HIGH-1 probes.
		{"read_file by Path to a key asks", "read_file", map[string]any{"Path": "/home/u/.ssh/id_rsa"}, parity.VerdictAsk},
		{"read_file by FilePath to .env asks", "read_file", map[string]any{"FilePath": "/repo/.env"}, parity.VerdictAsk},
		{"read_file by Path in the repo asks", "read_file", map[string]any{"Path": "/repo/a.go"}, parity.VerdictAsk},
		// #144 re-review, MEDIUM-A probes.
		{"read_file by file URI asks", "read_file", map[string]any{"Uri": "file:///home/u/.ssh/id_rsa", "Cwd": "/repo"}, parity.VerdictAsk},
		{"read_file by name and folder asks", "read_file", map[string]any{"File": "id_rsa", "Dir": "/repo"}, parity.VerdictAsk},
		{"read_file by Windows path asks", "read_file", map[string]any{"Path": "C:\\Users\\u\\.ssh\\id_rsa", "Cwd": "/repo"}, parity.VerdictAsk},
		{"unknown read_url of a file URI asks", "read_url", map[string]any{"Url": "file:///home/u/.ssh/id_rsa"}, parity.VerdictAsk},
		{"unknown open_resource of .env asks", "open_resource", map[string]any{"Uri": "file:///repo/.env"}, parity.VerdictAsk},
		{"unknown with a key path asks", "batch", map[string]any{"Files": map[string]any{"/home/u/.ssh/id_rsa": "read"}}, parity.VerdictAsk},
		{"unknown name beside a folder outside asks", "fetch_file", map[string]any{"Name": "id_rsa", "Folder": "/home/u/.ssh"}, parity.VerdictAsk},
		{"unknown name beside the repo", "summarize", map[string]any{"Name": "notes", "Folder": "/repo/docs"}, parity.VerdictNone},
		{"unknown https URL only", "read_url", map[string]any{"Url": "https://example.com/a"}, parity.VerdictNone},
		{"unknown write to the hooks file asks", "write_file", map[string]any{"Path": "/repo/.agents/hooks.json", "Content": "{}"}, parity.VerdictAsk},
		{"unknown tool on a path outside asks", "edit_file", map[string]any{"Path": "/etc/hosts"}, parity.VerdictAsk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := JudgeAntigravity(p, agyPayload(t, tt.tool, tt.args)); got.Verdict != tt.want {
				t.Fatalf("got %+v, want %s", got, tt.want)
			}
		})
	}
	if JudgeAntigravity(p, []byte("x")).Verdict != parity.VerdictAsk {
		t.Error("an unreadable payload did not ask")
	}
	if AntigravityJudged(agyPayload(t, "browser_open", nil)) || !AntigravityJudged([]byte("x")) {
		t.Error("AntigravityJudged")
	}
}

func TestAntigravityOutput(t *testing.T) {
	for _, v := range []parity.Verdict{parity.VerdictNone, parity.VerdictAllow} {
		if out := AntigravityOutput(parity.CallDecision{Verdict: v}); out != nil {
			t.Errorf("%s = %q, want nothing", v, out)
		}
	}
	for _, tt := range []struct {
		d    parity.CallDecision
		want string
	}{
		{parity.CallDecision{Verdict: parity.VerdictDeny, Group: "destructive"}, "deny"},
		{parity.CallDecision{Verdict: parity.VerdictAsk, Group: "destructive"}, "force_ask"},
		{parity.CallDecision{Verdict: parity.VerdictAsk, Reason: parity.ReasonProtected}, "force_ask"},
	} {
		var m map[string]string
		if err := json.Unmarshal(AntigravityOutput(tt.d), &m); err != nil || m["decision"] != tt.want || m["reason"] == "" {
			t.Errorf("%+v = %v, %v", tt.d, m, err)
		}
	}
	var m map[string]string
	if err := json.Unmarshal(AntigravityFallback(), &m); err != nil || m["decision"] != "force_ask" {
		t.Errorf("fallback = %v, %v", m, err)
	}
}
