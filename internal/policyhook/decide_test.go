package policyhook

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
)

const force = "git push --force origin main"

func plan() DecidePlan {
	d := NewDecidePlan("claude", "2.1.322", false, parity.PolicyPlan{OnScreen: true}, parity.PreApproved{Commands: []string{force}})
	d.Root, d.Protected = "/repo", []string{"/home/u/.aggy"}
	return d
}

func payload(t *testing.T, tool string, input map[string]any, extra map[string]any) []byte {
	t.Helper()
	m := map[string]any{"session_id": "s", "hook_event_name": "PreToolUse", "tool_name": tool, "tool_input": input, "cwd": "/repo"}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestJudgeUnreadable (T6976): a payload that is missing, not JSON, not an
// object, oversized (the caller passes nil), names a tool the hook does not
// know, or lacks the tool's field asks on screen and denies where no prompt
// can show. It never allows.
func TestJudgeUnreadable(t *testing.T) {
	for name, p := range map[string][]byte{
		"missing":       nil,
		"empty":         []byte(""),
		"not JSON":      []byte("git push --force origin main"),
		"array":         []byte(`["Bash"]`),
		"null":          []byte(`null`),
		"no tool name":  []byte(`{"tool_input":{"command":"ls"}}`),
		"no command":    payload(t, "Bash", map[string]any{}, nil),
		"monitor, none": payload(t, "Monitor", map[string]any{"description": "x"}, nil),
		"unknown, num":  payload(t, "mcp__x__run", map[string]any{"command": 3}, nil),
		"command array": payload(t, "Bash", map[string]any{"command": []string{force}}, nil),
		"empty command": payload(t, "Bash", map[string]any{"command": ""}, nil),
		"no file_path":  payload(t, "Read", map[string]any{"path": "/x/.env"}, nil),
		"grep path num": payload(t, "Grep", map[string]any{"path": 3}, nil),
	} {
		calls, mode, ok := ClaudeCalls(p)
		if ok {
			t.Errorf("%s: read as %+v", name, calls)
		}
		d := Judge(plan(), calls, mode, ok)
		if d.Verdict != parity.VerdictAsk || d.Reason != parity.ReasonUnknownInput {
			t.Errorf("%s: %+v, want ask", name, d)
		}
		np := plan()
		np.NoPrompt = true
		if d := Judge(np, calls, mode, ok); d.Verdict != parity.VerdictDeny {
			t.Errorf("%s, no prompt: %+v, want deny", name, d)
		}
	}
}

// TestJudgeIgnoresOtherFields (T6976): nothing in the payload but the tool
// input pre-approves: the working directory, session, transcript, an
// "allow" field, or another permission mode change nothing, and a mode that
// shows no prompt only turns an ask into a deny.
func TestJudgeIgnoresOtherFields(t *testing.T) {
	base := judgeP(t, plan(), payload(t, "Bash", map[string]any{"command": "rm -rf dist"}, nil))
	if base.Verdict != parity.VerdictAsk {
		t.Fatalf("base %+v", base)
	}
	for name, extra := range map[string]map[string]any{
		"cwd elsewhere":    {"cwd": "/other/repo"},
		"cwd root":         {"cwd": "/"},
		"accept edits":     {"permission_mode": "acceptEdits"},
		"auto":             {"permission_mode": "auto"},
		"plan":             {"permission_mode": "plan"},
		"decision field":   {"permissionDecision": "allow"},
		"pre-approved":     {"preapproved": []string{"rm -rf dist"}},
		"transcript":       {"transcript_path": "/tmp/x"},
		"hook output":      {"hookSpecificOutput": map[string]any{"permissionDecision": "allow"}},
		"approved command": {"command": "rm -rf dist"},
	} {
		if got := judgeP(t, plan(), payload(t, "Bash", map[string]any{"command": "rm -rf dist"}, extra)); got != base {
			t.Errorf("%s: %+v, want %+v", name, got, base)
		}
	}
	for _, mode := range []string{"bypassPermissions", "dontAsk"} {
		got := judgeP(t, plan(), payload(t, "Bash", map[string]any{"command": "rm -rf dist"}, map[string]any{"permission_mode": mode}))
		if got.Verdict != parity.VerdictDeny || got.Reason != parity.ReasonNoPrompt {
			t.Errorf("%s: %+v, want deny", mode, got)
		}
		allowed := judgeP(t, plan(), payload(t, "Bash", map[string]any{"command": force}, map[string]any{"permission_mode": mode}))
		if allowed.Verdict != parity.VerdictAllow {
			t.Errorf("%s, pre-approved: %+v, want allow", mode, allowed)
		}
	}
}

// judgeP reads payload p, which must be readable, and judges it by d.
func judgeP(t *testing.T, d DecidePlan, p []byte) parity.CallDecision {
	t.Helper()
	calls, mode, ok := ClaudeCalls(p)
	if !ok {
		t.Fatalf("unreadable: %s", p)
	}
	return Judge(d, calls, mode, ok)
}

// TestJudgeTools (T6976): each tool aggy's hook is matched to is judged, and
// only a Read honours a read pre-approval.
func TestJudgeTools(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "k.pem")
	if err := os.WriteFile(key, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := FileIdentity(root, key)
	if err != nil {
		t.Fatal(err)
	}
	p := NewDecidePlan("claude", "", false, parity.PolicyPlan{OnScreen: true},
		parity.PreApproved{Commands: []string{force}, Reads: []parity.ReadApproval{id}})
	p.Root = root
	for _, tt := range []struct {
		tool  string
		input map[string]any
		want  parity.Verdict
	}{
		{"Bash", map[string]any{"command": force}, parity.VerdictAllow},
		{"Bash", map[string]any{"command": force + " && true"}, parity.VerdictAsk},
		{"Bash", map[string]any{"command": "ls"}, parity.VerdictNone},

		{"Monitor", map[string]any{"command": "rm -rf ~", "description": "x"}, parity.VerdictAsk},
		{"Monitor", map[string]any{"command": force}, parity.VerdictAllow},
		{"mcp__shell__run", map[string]any{"command": "git push --force origin dev"}, parity.VerdictAsk},
		{"SomeNewTool", map[string]any{"file_path": "/repo/.env"}, parity.VerdictAsk},
		{"SomeNewTool", map[string]any{"path": "/repo/id_rsa"}, parity.VerdictAsk},
		{"SomeNewTool", map[string]any{"path": key}, parity.VerdictAsk},
		{"WebFetch", map[string]any{"url": "https://example.com/.env"}, parity.VerdictNone},
		{"Read", map[string]any{"file_path": key}, parity.VerdictAllow},
		{"Read", map[string]any{"file_path": "/repo/.env"}, parity.VerdictAsk},
		{"Edit", map[string]any{"file_path": key}, parity.VerdictAsk},
		{"Write", map[string]any{"file_path": "/repo/.env.local"}, parity.VerdictAsk},
		{"MultiEdit", map[string]any{"file_path": "/repo/id_rsa"}, parity.VerdictAsk},
		{"NotebookEdit", map[string]any{"notebook_path": "/repo/x.key"}, parity.VerdictAsk},
		{"Grep", map[string]any{"pattern": "secret", "path": "/repo/k.pem"}, parity.VerdictAsk},
		{"Grep", map[string]any{"pattern": ".env", "glob": "*.go"}, parity.VerdictNone},
		{"Glob", map[string]any{"pattern": "**/.env*"}, parity.VerdictAsk},
		{"Glob", map[string]any{"pattern": "**/*.go"}, parity.VerdictNone},
	} {
		calls, mode, ok := ClaudeCalls(payload(t, tt.tool, tt.input, nil))
		if got := Judge(p, calls, mode, ok); got.Verdict != tt.want {
			t.Errorf("%s %v: %+v, want %s", tt.tool, tt.input, got, tt.want)
		}
	}
}

// TestClaudeOutput: the hook's stdout is a PreToolUse decision whose reason
// holds no tool input, and nothing for no opinion.
func TestClaudeOutput(t *testing.T) {
	if out := ClaudeOutput(parity.CallDecision{Verdict: parity.VerdictNone}); out != nil {
		t.Errorf("no opinion printed %q", out)
	}
	for _, d := range []parity.CallDecision{
		{Verdict: parity.VerdictAllow, Group: "force-push", Reason: parity.ReasonPreApproved},
		{Verdict: parity.VerdictAsk, Group: "rm-rf", Reason: parity.ReasonPolicyMatch},
		{Verdict: parity.VerdictAsk, Reason: parity.ReasonUnknownInput},
		{Verdict: parity.VerdictDeny, Group: "reset-hard", Reason: parity.ReasonGroupDenied},
		{Verdict: parity.VerdictDeny, Group: "rm-rf", Reason: parity.ReasonNoPrompt},
	} {
		var v struct {
			H struct {
				Event, Decision, Reason string
			}
		}
		var raw map[string]map[string]string
		if err := json.Unmarshal(ClaudeOutput(d), &raw); err != nil {
			t.Fatal(err)
		}
		v.H.Event, v.H.Decision, v.H.Reason = raw["hookSpecificOutput"]["hookEventName"], raw["hookSpecificOutput"]["permissionDecision"], raw["hookSpecificOutput"]["permissionDecisionReason"]
		if v.H.Event != "PreToolUse" || v.H.Decision != string(d.Verdict) || v.H.Reason == "" {
			t.Errorf("%+v: %+v", d, v.H)
		}
	}
}

// TestDecideCommand (T6976): the wrapper runs aggy's hook and, when it fails,
// prints a fixed decision: ask, or deny where no prompt can show. Neither
// output depends on anything but that choice, and both are valid decisions.
func TestDecideCommand(t *testing.T) {
	sum := PlanSum([]byte("{}"))
	for _, noPrompt := range []bool{false, true} {
		cmd, err := DecideCommand("/usr/local/bin/aggy", "/home/u/.aggy/run/x/policy-decide.json", sum, noPrompt)
		if err != nil {
			t.Fatal(err)
		}
		want := "ask"
		if noPrompt {
			want = "deny"
		}
		out := FallbackOutput(noPrompt)
		args := " --vendor claude --plan /home/u/.aggy/run/x/policy-decide.json --sha256 " + sum
		if !strings.HasPrefix(cmd, "out=$(/usr/local/bin/aggy hook decide"+args+`) && printf '%s' "$out" || { `) ||
			!strings.Contains(cmd, "hook decide-failed"+args+" </dev/null >/dev/null 2>&1 & ") ||
			!strings.HasSuffix(cmd, "printf '%s\\n' '"+out+"'; }") {
			t.Errorf("command %q", cmd)
		}
		var v map[string]map[string]string
		if err := json.Unmarshal([]byte(out), &v); err != nil || v["hookSpecificOutput"]["permissionDecision"] != want ||
			!strings.HasPrefix(v["hookSpecificOutput"]["permissionDecisionReason"], "aggy's safety check failed") {
			t.Errorf("fallback %q: %v %v", out, v, err)
		}
		if strings.ContainsAny(out, "'$`\\") && strings.Contains(out, "'") {
			t.Errorf("fallback %q holds a quote the shell would end at", out)
		}
	}
	if strings.Contains(FallbackOutput(false), "deny") || !strings.Contains(FallbackOutput(true), "deny") {
		t.Error("fallback outputs mixed up")
	}
	for _, bad := range [][3]string{{"/a b/aggy", "/p", sum}, {"/bin/aggy", "/a'b/p", sum}, {"/bin/aggy", "rel/p", sum}, {"/bin/aggy", "/p;x", sum},
		{"/bin/aggy", "/p", "abc"}, {"/bin/aggy", "/p", strings.ToUpper(sum)}, {"/bin/aggy", "/p", sum[:63] + ";"}} {
		if _, err := DecideCommand(bad[0], bad[1], bad[2], false); err == nil {
			t.Errorf("DecideCommand(%q, %q, %q) took it", bad[0], bad[1], bad[2])
		}
	}
}

// TestCheckPlan (PR #129 review, HIGH-1): a plan is used only when its bytes
// hash to the sum the launch put in the hook's command; a plan rewritten to
// pre-approve a command, by a valid plan or not, is ErrPlanChanged.
func TestCheckPlan(t *testing.T) {
	good, err := json.Marshal(plan())
	if err != nil {
		t.Fatal(err)
	}
	sum := PlanSum(good)
	if d, err := CheckPlan(good, sum); err != nil || d.Root != "/repo" {
		t.Fatalf("good: %+v %v", d, err)
	}
	evil := plan()
	evil.Commands = append(evil.Commands, "git push --force origin dev")
	eb, _ := json.Marshal(evil)
	for name, b := range map[string][]byte{"rewritten": eb, "empty": nil, "spaced": append(good, ' ')} {
		if _, err := CheckPlan(b, sum); !errors.Is(err, ErrPlanChanged) {
			t.Errorf("%s: %v, want ErrPlanChanged", name, err)
		}
	}
	if _, err := CheckPlan(good, "nope"); !errors.Is(err, ErrPlanChanged) {
		t.Errorf("bad sum: %v", err)
	}
	for _, mode := range []string{"", "default", "acceptEdits"} {
		if d := PlanFailed(true, mode); d.Verdict != parity.VerdictAsk || d.Reason != parity.ReasonPlanChanged {
			t.Errorf("%q: %+v", mode, d)
		}
	}
	// PR #129 re-review: refused where no prompt shows, saying what failed;
	// and a call the hook would not judge (a web fetch) gets no opinion.
	d := PlanFailed(true, "dontAsk")
	if out := string(ClaudeOutput(d)); d.Verdict != parity.VerdictDeny || !strings.Contains(out, "decision plan") || strings.Contains(out, "unreadable call") {
		t.Errorf("dontAsk: %+v %s", d, out)
	}
	if d := PlanFailed(false, "dontAsk"); d.Verdict != parity.VerdictNone {
		t.Errorf("not judged: %+v", d)
	}
}

// TestOtherToolNeverAllowed (PR #129 re-review, confirmed on f003bef): an MCP
// tool carrying an exactly pre-approved command, or a pre-approved read path,
// is never allowed: the pre-approval is for Bash or Monitor, and for Read.
// What would be an allow is no opinion; an ask stays an ask.
func TestOtherToolNeverAllowed(t *testing.T) {
	root := t.TempDir()
	key := filepath.Join(root, "k.pem")
	if err := os.WriteFile(key, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := FileIdentity(root, key)
	if err != nil {
		t.Fatal(err)
	}
	p := plan()
	p.Root, p.Commands = root, []string{"rm -rf node_modules"}
	p.Reads = []parity.ReadApproval{id}
	for _, tt := range []struct {
		tool  string
		input map[string]any
		want  parity.Verdict
	}{
		{"Bash", map[string]any{"command": "rm -rf node_modules"}, parity.VerdictAllow},
		{"Monitor", map[string]any{"command": "rm -rf node_modules"}, parity.VerdictAllow},
		{"Read", map[string]any{"file_path": key}, parity.VerdictAllow},
		{"mcp__ssh__exec", map[string]any{"command": "rm -rf node_modules", "host": "prod"}, parity.VerdictNone},
		{"mcp__ssh__exec", map[string]any{"command": "rm -rf /", "host": "prod"}, parity.VerdictAsk},
		{"mcp__fs__read", map[string]any{"path": key}, parity.VerdictAsk},
		{"mcp__fs__read", map[string]any{"file_path": key}, parity.VerdictAsk},
	} {
		if got := judgeP(t, p, payload(t, tt.tool, tt.input, nil)); got.Verdict != tt.want {
			t.Errorf("%s %v: %+v, want %s", tt.tool, tt.input, got, tt.want)
		}
	}
}

// TestJudgeProtected (PR #129 review, HIGH-1 and HIGH-4): a write to, or a
// shell command naming, aggy's state folder (which holds the plan), its
// permissions file, or a Claude settings file asks, in any spelling the review
// named, and wins over a pre-approval; a group set to deny still refuses.
func TestJudgeProtected(t *testing.T) {
	root := t.TempDir()
	aggyHome := filepath.Join(t.TempDir(), ".aggy")
	if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, ".claude"), filepath.Join(root, "c")); err != nil {
		t.Fatal(err)
	}
	p := plan()
	p.Root, p.Protected = root, []string{aggyHome}
	planFile := filepath.Join(aggyHome, "run", "p.x.memory", "policy-decide.json")
	p.Commands = append(p.Commands, "echo hi > "+planFile)
	for _, tt := range []struct {
		tool  string
		input map[string]any
	}{
		{"Write", map[string]any{"file_path": planFile}},
		{"Edit", map[string]any{"file_path": filepath.Join(aggyHome, "permissions.json")}},
		{"Write", map[string]any{"file_path": filepath.Join(root, ".claude", "settings.local.json")}},
		{"Write", map[string]any{"file_path": ".claude/settings.local.json"}},
		{"Write", map[string]any{"file_path": "./.claude/../.claude/settings.json"}},
		{"Write", map[string]any{"file_path": filepath.Join(root, ".CLAUDE", "Settings.Local.JSON")}},
		{"Write", map[string]any{"file_path": filepath.Join(root, "sub", "..", ".claude", "settings.json")}},
		{"Write", map[string]any{"file_path": filepath.Join(root, "c", "settings.local.json")}},
		{"MultiEdit", map[string]any{"file_path": "/home/u/.claude/settings.json"}},
		{"NotebookEdit", map[string]any{"notebook_path": filepath.Join(aggyHome, "x.ipynb")}},
		{"Bash", map[string]any{"command": `echo '{"disableAllHooks":true}' > .claude/settings.local.json`}},
		{"Bash", map[string]any{"command": "echo hi > " + planFile}},
		{"Bash", map[string]any{"command": "cd ~/.aggy && echo x > permissions.json"}},
		{"Bash", map[string]any{"command": `cp x ".cla"ude/sett"ings.json"`}},
		{"Bash", map[string]any{"command": "rm ~/.aggy/run/*/policy-decide.json"}},
		{"Monitor", map[string]any{"command": "tee .claude/settings.local.json"}},
	} {
		got := judgeP(t, p, payload(t, tt.tool, tt.input, nil))
		if got.Verdict != parity.VerdictAsk {
			t.Errorf("%s %v: %+v, want ask", tt.tool, tt.input, got)
		}
	}
	for _, tt := range []struct {
		tool  string
		input map[string]any
	}{
		{"Write", map[string]any{"file_path": filepath.Join(root, "main.go")}},
		{"Write", map[string]any{"file_path": filepath.Join(root, ".vscode", "launch.json")}},
		{"Bash", map[string]any{"command": "go test ./..."}},

		{"Bash", map[string]any{"command": "cat .aggy/memory.md && npm run build"}},
		{"Bash", map[string]any{"command": "ls ~/.aggy/runner"}},
		{"Read", map[string]any{"file_path": filepath.Join(root, ".claude", "settings.json")}},
	} {
		if got := judgeP(t, p, payload(t, tt.tool, tt.input, nil)); got.Verdict != parity.VerdictNone {
			t.Errorf("%s %v: %+v, want no opinion", tt.tool, tt.input, got)
		}
	}
	deny := p
	deny.Settings = map[string]string{permissions.PolicyRmRf: "deny"}
	if got := judgeP(t, deny, payload(t, "Bash", map[string]any{"command": "rm -rf " + aggyHome}, nil)); got.Verdict != parity.VerdictDeny {
		t.Errorf("deny group: %+v", got)
	}
}

// TestJudgeTrustRecords (T6977, PR #130 review): aggy's trust records,
// trust.json and opencode-trust.json, are protected as its permissions file
// is: a write to either, or a shell command naming either by its absolute
// path, under ~/.aggy or $AGGY_HOME, or by name after naming aggy's folder,
// asks; a file of that name elsewhere, or another file in .aggy, does not.
func TestJudgeTrustRecords(t *testing.T) {
	root := t.TempDir()
	aggyHome := filepath.Join(t.TempDir(), "state")
	p := plan()
	p.Root, p.Protected = root, []string{aggyHome}
	for _, name := range []string{"trust.json", "opencode-trust.json"} {
		abs := filepath.Join(aggyHome, name)
		for _, tt := range []struct {
			tool  string
			input map[string]any
		}{
			{"Write", map[string]any{"file_path": abs}},
			{"Edit", map[string]any{"file_path": abs}},
			{"Bash", map[string]any{"command": "echo '{}' > " + abs}},
			{"Bash", map[string]any{"command": "echo '{}' > ~/.aggy/" + name}},
			{"Bash", map[string]any{"command": "cp x ~/.AGGY/" + name}},
			{"Bash", map[string]any{"command": "echo '{}' > \"$AGGY_HOME/" + name + "\""}},
			{"Bash", map[string]any{"command": "echo '{}' > ${AGGY_HOME}/" + name}},
			{"Bash", map[string]any{"command": "cd ~/.aggy && echo '{}' > " + name}},
			{"Bash", map[string]any{"command": `cd "$AGGY_HOME"; sed -i '' s/a/b/ ` + name}},
		} {
			if got := judgeP(t, p, payload(t, tt.tool, tt.input, nil)); got.Verdict != parity.VerdictAsk || got.Reason != parity.ReasonProtected {
				t.Errorf("%s %v: %+v, want ask", tt.tool, tt.input, got)
			}
		}
	}
	for _, tt := range []struct {
		tool  string
		input map[string]any
	}{
		{"Bash", map[string]any{"command": "cat .aggy/memory.md && npm run build"}},
		{"Bash", map[string]any{"command": "cat trust.json"}},
		{"Bash", map[string]any{"command": "cat ~/.aggy/trust.jsonl"}},
		{"Write", map[string]any{"file_path": filepath.Join(root, "trust.json")}},
		{"Write", map[string]any{"file_path": filepath.Join(root, "config", "opencode-trust.json")}},
	} {
		if got := judgeP(t, p, payload(t, tt.tool, tt.input, nil)); got.Verdict != parity.VerdictNone {
			t.Errorf("%s %v: %+v, want no opinion", tt.tool, tt.input, got)
		}
	}
}

// TestReadIdentity (PR #129 review, MEDIUM-4): a read pre-approval allows only
// the same file at the same path: not a symlink put in its place or on its
// path, not a replaced file, not a path outside the root.
//
// T6976 (CI on ubuntu): device and inode alone allowed a replaced file, since
// Linux gave the new file the inode number the removed one freed. The cases
// below do not depend on whether this OS reuses an inode: the replaced file
// has another size, and the reuse is forced by a recorded identity with the
// file's own device and inode but other size and times.
func TestReadIdentity(t *testing.T) {
	root := t.TempDir()
	write := func(p, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	key, other := filepath.Join(root, "certs", "server.key"), filepath.Join(root, "other.key")
	write(key, "k")
	write(other, "k")
	id, err := FileIdentity(root, key)
	if err != nil {
		t.Fatal(err)
	}
	readAs := func(a parity.ReadApproval) parity.Verdict {
		d := plan()
		d.Root, d.Reads = root, []parity.ReadApproval{a}
		return judgeP(t, d, payload(t, "Read", map[string]any{"file_path": key}, nil)).Verdict
	}
	read := func() parity.Verdict { return readAs(id) }
	if v := read(); v != parity.VerdictAllow {
		t.Fatalf("same file: %s", v)
	}
	// The recorded identity differs from the file in one field, or (inode
	// reuse) in everything but device and inode.
	for _, tt := range []struct {
		name string
		edit func(*parity.ReadApproval)
	}{
		{"inode reused", func(a *parity.ReadApproval) { a.Size, a.ModNS, a.ChangeNS = a.Size+1, a.ModNS-1, a.ChangeNS-1 }},
		{"device", func(a *parity.ReadApproval) { a.Dev++ }},
		{"inode", func(a *parity.ReadApproval) { a.Ino++ }},
		{"size", func(a *parity.ReadApproval) { a.Size++ }},
		{"modification time", func(a *parity.ReadApproval) { a.ModNS-- }},
		{"change time", func(a *parity.ReadApproval) { a.ChangeNS-- }},
	} {
		a := id
		tt.edit(&a)
		if v := readAs(a); v != parity.VerdictAsk {
			t.Errorf("%s differs: %s", tt.name, v)
		}
	}
	// A symlink in its place, to another key.
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, key); err != nil {
		t.Fatal(err)
	}
	if v := read(); v != parity.VerdictAsk {
		t.Errorf("a symlink in its place: %s", v)
	}
	// A symlinked folder on its path.
	_ = os.Remove(key)
	if err := os.Rename(filepath.Join(root, "certs"), filepath.Join(root, "real")); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, "real", "server.key"), "k")
	if err := os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "certs")); err != nil {
		t.Fatal(err)
	}
	if v := read(); v != parity.VerdictAsk {
		t.Errorf("a symlink on its path: %s", v)
	}
	// A replaced file at the same path, which may well get the removed file's
	// inode on Linux: its size differs, so this asks whatever the inode.
	_ = os.Remove(filepath.Join(root, "certs"))
	write(key, "another key")
	if v := read(); v != parity.VerdictAsk {
		t.Errorf("a replaced file: %s", v)
	}
	if _, err := FileIdentity(root, filepath.Join(filepath.Dir(root), "x.key")); err == nil {
		t.Error("a path outside the root has an identity")
	}
}

// TestParseDecidePlan: a plan with an unknown key, another version, text
// after it, or a setting aggy does not know is refused. A group set to allow
// (turned off on screen, T6979) is taken, and the hook has no opinion on it.
func TestParseDecidePlan(t *testing.T) {
	gp := NewDecidePlan("claude", "", true, parity.PolicyPlan{OnScreen: true,
		Settings: map[string]permissions.Kind{permissions.PolicyRmRf: permissions.Deny}}, parity.PreApproved{Commands: []string{"x"}})
	gp.Root = "/repo"
	good, err := json.Marshal(gp)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseDecidePlan(good)
	if err != nil || d.VendorVersion != "unknown" || !d.NoPrompt || d.Settings[permissions.PolicyRmRf] != "deny" {
		t.Fatalf("good: %+v %v", d, err)
	}
	for _, bad := range []string{
		`{"version":2,"vendor":"claude","root":"/repo","extra":1}`,
		`{"version":1,"vendor":"claude","root":"/repo"}`,
		`{"version":2,"vendor":"claude","root":"/repo"} {}`,
		`{"version":2,"vendor":"claude","root":"/repo","settings":{"rm-rf":"maybe"}}`,
		`{"version":2,"vendor":"claude","root":"/repo","settings":{"nope":"ask"}}`,
		`{"version":2,"vendor":"claude"}`,
		`{"version":2,"vendor":"claude","root":"repo"}`,
		`[]`, ``,
	} {
		if _, err := ParseDecidePlan([]byte(bad)); err == nil {
			t.Errorf("took %s", bad)
		}
	}
	off, err := ParseDecidePlan([]byte(`{"version":2,"vendor":"claude","root":"/repo","settings":{"rm-rf":"allow"}}`))
	if err != nil {
		t.Fatalf("a group turned off: %v", err)
	}
	for _, tc := range []struct {
		cmd  string
		want parity.Verdict
	}{
		{"rm -rf build", parity.VerdictNone},
		{"git reset --hard", parity.VerdictAsk},
	} {
		if d := Judge(off, []parity.Call{{Kind: parity.CallShell, Text: tc.cmd}}, "", true); d.Verdict != tc.want {
			t.Errorf("%s with rm-rf off: %+v, want %s", tc.cmd, d, tc.want)
		}
	}
	// Turning a group off never turns off the protected paths' asks (#129).
	off.Protected = []string{"/home/me/.aggy"}
	if d := Judge(off, []parity.Call{{Kind: parity.CallShell, Text: "rm -rf /home/me/.aggy"}}, "", true); d.Verdict != parity.VerdictAsk {
		t.Errorf("a protected path with rm-rf off: %+v, want ask", d)
	}
}

// TestOpenCodeJudge (T6976): only the shell action is judged; several
// resources are never allowed together; no resource asks.
func TestOpenCodeJudge(t *testing.T) {
	p := plan()
	for _, tt := range []struct {
		in   string
		want parity.Verdict
	}{
		{`{"action":"shell","resources":["` + force + `"],"effect":"ask"}`, parity.VerdictAllow},
		{`{"action":"shell","resources":["` + force + `","echo hi"],"effect":"ask"}`, parity.VerdictAsk},
		{`{"action":"shell","resources":["` + force + `","` + force + `"],"effect":"ask"}`, parity.VerdictAsk},
		{`{"action":"shell","resources":["rm -rf x"],"effect":"allow"}`, parity.VerdictAsk},
		{`{"action":"shell","resources":["ls"],"effect":"allow"}`, parity.VerdictNone},
		{`{"action":"shell","resources":[],"effect":"ask"}`, parity.VerdictAsk},
		{`{"action":"shell","resources":[1],"effect":"ask"}`, parity.VerdictAsk},
		{`{"action":"read","resources":["/x/.env"],"effect":"allow"}`, parity.VerdictAsk},
		{`{"action":"read","resources":["certs/id_ed25519"],"effect":"allow"}`, parity.VerdictAsk},
		{`{"action":"read","resources":["main.go"],"effect":"allow"}`, parity.VerdictNone},
		{`{"action":"edit","resources":["/x/.env"],"effect":"allow"}`, parity.VerdictNone},
		{`not json`, parity.VerdictAsk},
		// T6979: a read outside the root asks, so the plugin can clear it.
		{`{"action":"read","resources":["/elsewhere/notes.txt"],"effect":"allow"}`, parity.VerdictAsk},
		{`{"action":"read","resources":["/repo/../elsewhere/notes.txt"],"effect":"allow"}`, parity.VerdictAsk},
		{`{"action":"read","resources":["/repository/notes.txt"],"effect":"allow"}`, parity.VerdictAsk},
		{`{"action":"read","resources":["/repo/docs/a.md"],"effect":"allow"}`, parity.VerdictNone},
		{`{"action":"read","resources":["~/notes.txt"],"effect":"allow"}`, parity.VerdictNone},
		{`{"action":"shell","resources":["cat /elsewhere/notes.txt"],"effect":"allow"}`, parity.VerdictNone},
	} {
		calls, ok := OpenCodeCalls([]byte(tt.in), "/repo")
		if got := JudgeOpenCode(p, calls, ok); got.Verdict != tt.want {
			t.Errorf("%s: %+v, want %s", tt.in, got, tt.want)
		}
	}
	if got := JudgeOpenCode(p, []parity.Call{{Kind: parity.CallRead, Text: "/elsewhere/a"}}, true); got.Reason != parity.ReasonOutside {
		t.Errorf("outside read: %+v", got)
	}
	// A group set to deny still refuses outside the root.
	denied := plan()
	denied.Settings = map[string]string{permissions.PolicyEnv: "deny"}
	if got := JudgeOpenCode(denied, []parity.Call{{Kind: parity.CallRead, Text: "/elsewhere/.env"}}, true); got.Verdict != parity.VerdictDeny {
		t.Errorf("a denied group outside: %+v", got)
	}
	if string(OpenCodeOutput(parity.CallDecision{Verdict: parity.VerdictNone})) != "none\n" {
		t.Error("no opinion is not none")
	}
}
