package claude

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestCheckSettings (T6962): a person's --settings layer that would get past
// aggy's policy or hooks loosens; one that only adds or tightens passes.
func TestCheckSettings(t *testing.T) {
	tests := []struct {
		name    string
		layer   string
		loosens bool
	}{
		{"allow rules", `{"permissions":{"allow":["Bash(npm test)"]}}`, false},
		{"own deny rules and hooks", `{"permissions":{"deny":["Read(x)"]},"hooks":{"Stop":[]}}`, false},
		{"a mode that asks", `{"permissions":{"defaultMode":"plan"}}`, false},
		{"bypass turned off", `{"permissions":{"disableBypassPermissionsMode":"disable"}}`, false},
		{"hooks turned off, explicitly not", `{"disableAllHooks":false}`, false},
		{"a bypass mode", `{"permissions":{"defaultMode":"bypassPermissions"}}`, true},
		{"an unknown mode", `{"permissions":{"defaultMode":"newMode"}}`, true},
		{"a mode that is no string", `{"permissions":{"defaultMode":1}}`, true},
		{"hooks off", `{"disableAllHooks":true}`, true},
		{"a later dangerous key", `{"skipDangerousModePermissionPrompt":true}`, true},
		{"bypass anywhere as a value", `{"env":{"X":"bypassPermissions"}}`, true},
		// PR #124 review.
		{"project MCP servers trusted", `{"enableAllProjectMcpServers":true}`, true},
		{"only managed hooks", `{"allowManagedHooksOnly":true}`, true},
		{"only managed rules", `{"allowManagedPermissionRulesOnly":true}`, true},
		{"bypass inside a list", `{"hooks":{"Stop":[{"x":"bypassPermissions"}]}}`, true},
		{"a dangerous key inside a list", `{"a":[{"skipDangerousModePermissionPrompt":true}]}`, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ParseSettings([]byte(tt.layer))
			if err != nil {
				t.Fatal(err)
			}
			if err := CheckSettings(m); errors.Is(err, adapter.ErrLoosens) != tt.loosens {
				t.Errorf("CheckSettings = %v, loosens want %v", err, tt.loosens)
			}
		})
	}
	// Refused whatever --no-policy says: a credential or an endpoint.
	for _, layer := range []string{`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:1"}}`, `{"env":{"ANTHROPIC_API_KEY":"x"}}`,
		`{"env":{"ANTHROPIC_AUTH_TOKEN":"x"}}`, `{"apiKeyHelper":"/bin/echo x"}`} {
		m, err := ParseSettings([]byte(layer))
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckSettings(m); !errors.Is(err, ErrSettingsCredential) {
			t.Errorf("CheckSettings(%s) = %v, want a credential refusal", layer, err)
		}
	}
	if _, err := ParseSettings([]byte(`["x"]`)); err == nil {
		t.Error("a JSON array parsed as a settings layer")
	}
}

// TestInteractiveMergesSettings (T6962): the person's --settings layer goes in
// aggy's one inline layer, under aggy's: lists join, so aggy's deny rules and
// hooks stay, and aggy's value wins where both set one.
func TestInteractiveMergesSettings(t *testing.T) {
	user := map[string]any{
		"permissions": map[string]any{"allow": []any{"Bash(npm test)"}, "deny": []any{"Read(own)"}, "defaultMode": "plan"},
		"model":       "opus",
	}
	req := adapter.InteractiveRequest{Cwd: "/w", SessionID: sid, Deny: []string{"Bash(rm -rf*)"},
		Rules: adapter.SharedRules{Mode: "acceptEdits"}, UserSettings: user, Extra: []string{"-p", "hi"}}
	c, err := Adapter{}.Interactive(req)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.Index(c.Args, "--settings")
	if i < 0 || slices.Contains(c.Args[i+2:], "--settings") {
		t.Fatalf("args %q, want one --settings", c.Args)
	}
	var got struct {
		Permissions struct {
			Allow, Deny []string
			DefaultMode string
		}
		Model string
	}
	if err := json.Unmarshal([]byte(c.Args[i+1]), &got); err != nil {
		t.Fatal(err)
	}
	// PR #124 review (hand-edited): the person's plan, stricter than aggy's
	// acceptEdits, stands, and no note says it was replaced.
	if !slices.Equal(got.Permissions.Deny, []string{"Read(own)", "Bash(rm -rf*)"}) || !slices.Equal(got.Permissions.Allow, []string{"Bash(npm test)"}) ||
		got.Permissions.DefaultMode != "plan" || got.Model != "opus" {
		t.Errorf("layer = %s", c.Args[i+1])
	}
	if !slices.Equal(c.Args[len(c.Args)-2:], []string{"-p", "hi"}) {
		t.Errorf("args %q, want the person's words last", c.Args)
	}
	if o := SettingsOverrides(req); len(o) != 0 {
		t.Errorf("overrides = %q, want none", o)
	}
}

// TestSettingsOverrides (PR #124 review): a key of the person's --settings
// that aggy's own layer replaces is named for a note, unless it is a plan mode,
// which stands; with a SettingsPath, the layer goes in that file, not argv.
func TestSettingsOverrides(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want []string
	}{
		{"a looser mode is replaced", "acceptEdits", []string{"permissions.defaultMode"}},
		{"plan stands", "plan", nil},
		{"the same mode is no override", "default", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := adapter.InteractiveRequest{Cwd: "/w", SessionID: sid, Rules: adapter.SharedRules{Mode: "default"},
				UserSettings: map[string]any{"permissions": map[string]any{"defaultMode": tt.mode}}}
			if got := SettingsOverrides(req); !slices.Equal(got, tt.want) {
				t.Errorf("overrides = %q, want %q", got, tt.want)
			}
			req.SettingsPath = "/run/x/settings.json"
			c, err := Adapter{}.Interactive(req)
			if err != nil {
				t.Fatal(err)
			}
			if i := slices.Index(c.Args, "--settings"); i < 0 || c.Args[i+1] != req.SettingsPath {
				t.Errorf("args %q, want --settings %s", c.Args, req.SettingsPath)
			}
		})
	}
}

// TestJoinMCPConfig (T6962): aggy's MCP file joins the person's own
// --mcp-config where they wrote it, so the words after it keep their meaning;
// with none, it goes first, as before.
func TestJoinMCPConfig(t *testing.T) {
	tests := []struct {
		name  string
		extra []string
		want  []string
	}{
		{"none", []string{"hi"}, []string{"--mcp-config", "/run/mcp.json", "--session-id", sid, "hi"}},
		{"two words", []string{"--mcp-config", "./a.json", "--verbose"}, []string{"--session-id", sid, "--mcp-config", "/run/mcp.json", "./a.json", "--verbose"}},
		{"with =", []string{"--mcp-config=./a.json"}, []string{"--session-id", sid, "--mcp-config", "/run/mcp.json", "./a.json"}},
		{"after --, prompt text", []string{"--", "--mcp-config"}, []string{"--mcp-config", "/run/mcp.json", "--session-id", sid, "--", "--mcp-config"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Adapter{}.Interactive(adapter.InteractiveRequest{Cwd: "/w", SessionID: sid, MCPConfigFile: "/run/mcp.json", Extra: tt.extra})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(c.Args, tt.want) {
				t.Errorf("args %q, want %q", c.Args, tt.want)
			}
		})
	}
}
