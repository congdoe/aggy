package claude

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

func policyHookReq() adapter.InteractiveRequest {
	return adapter.InteractiveRequest{
		Deny: []string{"Bash(git reset --hard*)"},
		PolicyHook: &adapter.PolicyHook{
			Group: adapter.HookGroup{Matcher: "^(Bash|Read)$", Hooks: []adapter.HookCommand{{
				Type: "command", Command: "/bin/aggy hook decide --vendor claude --plan /p", Timeout: 10, OnFailure: "block"}}},
			Fallback: []string{"Bash(rm -rf*)", "Read(//**/.env)"},
		},
	}
}

type layerView struct {
	Permissions struct{ Ask, Deny []string }   `json:"permissions"`
	Hooks       map[string][]adapter.HookGroup `json:"hooks"`
}

func render(t *testing.T, req adapter.InteractiveRequest) layerView {
	t.Helper()
	js, ok, err := SettingsJSON(req)
	if err != nil || !ok {
		t.Fatalf("SettingsJSON: %v %v", ok, err)
	}
	var v layerView
	if err := json.Unmarshal(js, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestPolicyHookLayer (T6976): with the decision hook, the layer carries it
// under PreToolUse with its timeout and onFailure block, and no static asks;
// the denies stay. Whenever the final layer would not run it, the static asks
// come back: hooks turned off in the person's layer, or the hook missing.
func TestPolicyHookLayer(t *testing.T) {
	v := render(t, policyHookReq())
	if len(v.Permissions.Ask) != 0 || !slices.Equal(v.Permissions.Deny, []string{"Bash(git reset --hard*)"}) {
		t.Errorf("permissions %+v", v.Permissions)
	}
	pre := v.Hooks["PreToolUse"]
	if len(pre) != 1 || pre[0].Matcher != "^(Bash|Read)$" || pre[0].Hooks[0].OnFailure != "block" || pre[0].Hooks[0].Timeout != 10 {
		t.Errorf("PreToolUse %+v", pre)
	}

	// The tools' hooks keep their place, and aggy's goes last.
	req := policyHookReq()
	req.Hooks = map[string][]adapter.HookGroup{"PreToolUse": {{Matcher: "Bash", Hooks: []adapter.HookCommand{{Type: "command", Command: "x"}}}}}
	if pre := render(t, req).Hooks["PreToolUse"]; len(pre) != 2 || pre[0].Hooks[0].Command != "x" || pre[1].Matcher != "^(Bash|Read)$" {
		t.Errorf("with a tool's hook: %+v", pre)
	}

	for name, user := range map[string]map[string]any{
		"disableAllHooks":       {"disableAllHooks": true},
		"disableAllHooks other": {"disableAllHooks": "yes"},
		"allowManagedHooksOnly": {"allowManagedHooksOnly": true},
	} {
		req := policyHookReq()
		req.UserSettings = user
		if v := render(t, req); !slices.Equal(v.Permissions.Ask, req.PolicyHook.Fallback) {
			t.Errorf("%s: asks %q, want the static ones back", name, v.Permissions.Ask)
		}
	}
	req = policyHookReq()
	req.UserSettings = map[string]any{"disableAllHooks": false, "permissions": map[string]any{"ask": []any{"Bash(npm publish)"}}}
	if v := render(t, req); !slices.Equal(v.Permissions.Ask, []string{"Bash(npm publish)"}) {
		t.Errorf("hooks on: asks %q", v.Permissions.Ask)
	}
}

// TestPolicyHookRuns: the check the fallback rests on.
func TestPolicyHookRuns(t *testing.T) {
	g := policyHookReq().PolicyHook.Group
	b, _ := json.Marshal(map[string]any{"hooks": map[string]any{"PreToolUse": []adapter.HookGroup{g}}})
	var layer map[string]any
	_ = json.Unmarshal(b, &layer)
	if !PolicyHookRuns(layer, g) {
		t.Error("the hook is there")
	}
	other := g
	other.Matcher = "Bash"
	if PolicyHookRuns(layer, other) {
		t.Error("another matcher counted")
	}
	if PolicyHookRuns(map[string]any{}, g) {
		t.Error("an empty layer runs it")
	}
	layer["disableAllHooks"] = 1.0
	if PolicyHookRuns(layer, g) {
		t.Error("hooks off counted")
	}
}

// TestNoPromptLaunch: bypassPermissions and dontAsk, however set at launch.
func TestNoPromptLaunch(t *testing.T) {
	for _, tt := range []struct {
		extra []string
		user  map[string]any
		mode  string
		want  bool
	}{
		{nil, nil, "", false},
		{[]string{"--permission-mode", "dontAsk"}, nil, "", true},
		{[]string{"--permission-mode=bypassPermissions"}, nil, "", true},
		{[]string{"--permission-mode", "auto"}, nil, "", false},
		{[]string{"--dangerously-skip-permissions"}, nil, "", true},
		{[]string{"--", "--permission-mode", "dontAsk"}, nil, "", false},
		{nil, map[string]any{"permissions": map[string]any{"defaultMode": "dontAsk"}}, "", true},
		{nil, map[string]any{"permissions": map[string]any{"defaultMode": "acceptEdits"}}, "", false},
		{nil, nil, "dontAsk", true},
		{nil, nil, "bypassPermissions", true},
		{nil, nil, "plan", false},
	} {
		if got := NoPromptLaunch(tt.extra, tt.user, tt.mode); got != tt.want {
			t.Errorf("NoPromptLaunch(%q, %v, %q) = %v", tt.extra, tt.user, tt.mode, got)
		}
	}
	if !SkipsHooks([]string{"--model", "x", "--safe-mode"}) || !SkipsHooks([]string{"--bare"}) || SkipsHooks([]string{"--", "--bare"}) || SkipsHooks(nil) {
		t.Error("SkipsHooks")
	}
}
