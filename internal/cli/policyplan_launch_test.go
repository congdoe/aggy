package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
)

// TestChildPlan (PR #125 integration): one choice decides, for an on-screen
// launch, both whether the shared rules are read (gateLaunch's rules and
// addPermissions) and whether the safety policy may ask (policyPlan): the
// session's own child asks and reads them, a borrow's child reopened on screen
// does neither.
func TestChildPlan(t *testing.T) {
	tests := []struct {
		kind     string
		onScreen bool
	}{
		{"", true},
		{"image", false},
		{"video", false},
		{"research", false},
		{"news", false},
		{"review", false},
		{"edit", false},
	}
	for _, tt := range tests {
		who := childPlan(tt.kind)
		if (who == planOnScreen) != tt.onScreen {
			t.Errorf("childPlan(%q) = %v, want on screen %v", tt.kind, who, tt.onScreen)
		}
		if got := policyPlan(who, "", permissions.Set{}).OnScreen; got != tt.onScreen {
			t.Errorf("policyPlan(childPlan(%q)).OnScreen = %v, want %v", tt.kind, got, tt.onScreen)
		}
		ask, _ := parity.ClaudePolicy(policyPlan(who, "", permissions.Set{}).Decide())
		if (len(ask) > 0) != tt.onScreen {
			t.Errorf("kind %q: asks %q, want asks only on screen", tt.kind, ask)
		}
	}
}

// TestSettingsFileAsks (PR #125 integration): a person's --settings file on
// screen is merged into a file of aggy's (writeSettingsFile), and that file
// carries the policy's asks as well as its denies, as the inline layer does.
func TestSettingsFileAsks(t *testing.T) {
	isolate(t)
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "mine.json"), []byte(`{"permissions":{"allow":["Bash(npm test)"],"ask":[]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	layer, rest, _, fromFile, err := claudeSettings([]string{"--settings", "mine.json"}, cwd, false)
	if err != nil || !fromFile || len(rest) != 0 {
		t.Fatalf("claudeSettings: %v, file %v, rest %q", err, fromFile, rest)
	}
	req := adapter.InteractiveRequest{UserSettings: layer}
	// One group set to deny, so the plan has both asks and denies.
	set := permissions.Set{Policy: map[string]permissions.Kind{"reset-hard": permissions.Deny}}
	req.Ask, req.Deny = parity.ClaudePolicy(policyPlan(planOnScreen, cwd, set).Decide())
	if len(req.Ask) == 0 || len(req.Deny) == 0 {
		t.Fatalf("on screen: asks %q, denies %q; want both", req.Ask, req.Deny)
	}
	dir := filepath.Join(t.TempDir(), "mem")
	if err := writeSettingsFile(&req, gated{settingsFile: true}, dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(req.SettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Permissions struct{ Allow, Ask, Deny []string } `json:"permissions"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, a := range req.Ask {
		if !slices.Contains(got.Permissions.Ask, a) {
			t.Errorf("file asks %q lack %q", got.Permissions.Ask, a)
		}
	}
	for _, d := range req.Deny {
		if !slices.Contains(got.Permissions.Deny, d) {
			t.Errorf("file denies %q lack %q", got.Permissions.Deny, d)
		}
	}
	if !slices.Contains(got.Permissions.Allow, "Bash(npm test)") {
		t.Errorf("file allows %q lack the person's", got.Permissions.Allow)
	}
}
