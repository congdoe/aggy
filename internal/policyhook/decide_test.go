package policyhook

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
)

const force = "git push --force origin main"

func plan() DecidePlan {
	return NewDecidePlan("claude", "2.1.322", false, parity.PolicyPlan{OnScreen: true}, parity.PreApproved{Commands: []string{force}})
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
		"unknown tool":  payload(t, "WebFetch", map[string]any{"url": "x"}, nil),
		"no command":    payload(t, "Bash", map[string]any{}, nil),
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
	p := NewDecidePlan("claude", "", false, parity.PolicyPlan{OnScreen: true},
		parity.PreApproved{Commands: []string{force}, Reads: []string{"/repo/k.pem"}})
	for _, tt := range []struct {
		tool  string
		input map[string]any
		want  parity.Verdict
	}{
		{"Bash", map[string]any{"command": force}, parity.VerdictAllow},
		{"Bash", map[string]any{"command": force + " && true"}, parity.VerdictAsk},
		{"Bash", map[string]any{"command": "ls"}, parity.VerdictNone},
		{"Read", map[string]any{"file_path": "/repo/k.pem"}, parity.VerdictAllow},
		{"Read", map[string]any{"file_path": "/repo/.env"}, parity.VerdictAsk},
		{"Edit", map[string]any{"file_path": "/repo/k.pem"}, parity.VerdictAsk},
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
	for _, noPrompt := range []bool{false, true} {
		cmd, err := DecideCommand("/usr/local/bin/aggy", "/home/u/.aggy/run/x/policy-decide.json", noPrompt)
		if err != nil {
			t.Fatal(err)
		}
		want := "ask"
		if noPrompt {
			want = "deny"
		}
		out := FallbackOutput(noPrompt)
		if !strings.HasPrefix(cmd, "/usr/local/bin/aggy hook decide --vendor claude --plan /home/u/.aggy/run/x/policy-decide.json || { ") ||
			!strings.Contains(cmd, "hook decide-failed --vendor claude --plan /home/u/.aggy/run/x/policy-decide.json </dev/null >/dev/null 2>&1 & ") ||
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
	for _, bad := range [][2]string{{"/a b/aggy", "/p"}, {"/bin/aggy", "/a'b/p"}, {"/bin/aggy", "rel/p"}, {"/bin/aggy", "/p;x"}} {
		if _, err := DecideCommand(bad[0], bad[1], false); err == nil {
			t.Errorf("DecideCommand(%q, %q) took it", bad[0], bad[1])
		}
	}
}

// TestParseDecidePlan: a plan with an unknown key, another version, text
// after it, or a group set to allow is refused.
func TestParseDecidePlan(t *testing.T) {
	good, err := json.Marshal(NewDecidePlan("claude", "", true, parity.PolicyPlan{OnScreen: true,
		Settings: map[string]permissions.Kind{permissions.PolicyRmRf: permissions.Deny}}, parity.PreApproved{Commands: []string{"x"}}))
	if err != nil {
		t.Fatal(err)
	}
	d, err := ParseDecidePlan(good)
	if err != nil || d.VendorVersion != "unknown" || !d.NoPrompt || d.Settings[permissions.PolicyRmRf] != "deny" {
		t.Fatalf("good: %+v %v", d, err)
	}
	for _, bad := range []string{
		`{"version":1,"vendor":"claude","extra":1}`,
		`{"version":2,"vendor":"claude"}`,
		`{"version":1,"vendor":"claude"} {}`,
		`{"version":1,"vendor":"claude","settings":{"rm-rf":"allow"}}`,
		`{"version":1,"vendor":"claude","settings":{"nope":"ask"}}`,
		`[]`, ``,
	} {
		if _, err := ParseDecidePlan([]byte(bad)); err == nil {
			t.Errorf("took %s", bad)
		}
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
		{`{"action":"read","resources":["/x/.env"],"effect":"allow"}`, parity.VerdictNone},
		{`not json`, parity.VerdictAsk},
	} {
		calls, ok := OpenCodeCalls([]byte(tt.in))
		if got := JudgeOpenCode(p, calls, ok); got.Verdict != tt.want {
			t.Errorf("%s: %+v, want %s", tt.in, got, tt.want)
		}
	}
	if string(OpenCodeOutput(parity.CallDecision{Verdict: parity.VerdictNone})) != "none\n" {
		t.Error("no opinion is not none")
	}
}
