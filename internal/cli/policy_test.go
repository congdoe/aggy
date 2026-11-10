package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/probe"
)

func TestPolicyWire(t *testing.T) {
	h := isolate(t)
	path := filepath.Join(h, ".codex", "rules", "aggy.rules")
	out := mustRun(t, "policy", "wire", "--dry-run")
	if !strings.Contains(out, "+prefix_rule(") {
		t.Errorf("dry run shows no diff:\n%s", out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the dry run wrote the rules file")
	}
	mustRun(t, "policy", "wire")
	if b, err := os.ReadFile(path); err != nil || string(b) != parity.CodexRules {
		t.Fatalf("rules file = %q, %v", b, err)
	}
	if out := mustRun(t, "policy", "wire"); !strings.Contains(out, "Nothing to change") {
		t.Errorf("rewire: %s", out)
	}
	mustRun(t, "unwire", "--all")
	if _, err := os.Stat(filepath.Join(h, ".codex")); !os.IsNotExist(err) {
		t.Errorf("unwire left %s: %v", filepath.Join(h, ".codex"), err)
	}
	// A rules file of the person's is never replaced.
	writeAt(t, path, "prefix_rule(pattern = [\"ls\"])\n", 0o600)
	if code, _, errOut := run("policy", "wire"); code != 1 || !strings.Contains(errOut, "never replaces it") {
		t.Errorf("exit %d: %s", code, errOut)
	}
	if b, _ := os.ReadFile(path); string(b) != "prefix_rule(pattern = [\"ls\"])\n" {
		t.Errorf("the person's rules file changed: %q", b)
	}
	for _, args := range [][]string{{"policy"}, {"policy", "wire", "x"}, {"policy", "show"}} {
		if code, _, _ := run(args...); code != 2 {
			t.Errorf("%q: exit %d", args, code)
		}
	}
}

func TestAddPolicy(t *testing.T) {
	isolate(t)
	tests := []struct {
		v    adapter.Vendor
		caps []string
		deny []string // nil: the policy cannot reach this CLI
	}{
		{adapter.Claude, []string{probe.CapSettings}, parity.ClaudeDeny},
		{adapter.Claude, nil, nil},
		{adapter.Grok, []string{probe.CapDeny}, parity.GrokDeny},
		{adapter.Grok, []string{probe.CapRules}, nil},
		{adapter.Codex, []string{probe.CapConfigOverride}, nil},
	}
	// A borrow child's plan (the zero plan): every rule a deny, as before T6967.
	borrow := parity.PolicyPlan{}
	for _, tt := range tests {
		// Required (the default): a CLI the policy cannot reach is refused.
		var req adapter.InteractiveRequest
		warns, err := addPolicy(tt.v, tt.caps, true, borrow, &req)
		if !slices.Equal(req.Deny, tt.deny) || len(warns) > 0 || (err != nil) != (tt.deny == nil) {
			t.Errorf("required %s %v: deny %v, warns %v, err %v", tt.v, tt.caps, req.Deny, warns, err)
		}
		if err != nil && !strings.Contains(err.Error(), "--no-policy") {
			t.Errorf("the refusal does not name the way out: %v", err)
		}
		// Not required: a warning instead.
		req = adapter.InteractiveRequest{}
		warns, err = addPolicy(tt.v, tt.caps, false, borrow, &req)
		if !slices.Equal(req.Deny, tt.deny) || err != nil || (len(warns) == 0) != (tt.deny != nil) {
			t.Errorf("optional %s %v: deny %v, warns %v, err %v", tt.v, tt.caps, req.Deny, warns, err)
		}
	}
	// Codex is refused until the rules file is wired, then needs nothing
	// on its command line.
	mustRun(t, "policy", "wire")
	var req adapter.InteractiveRequest
	if warns, err := addPolicy(adapter.Codex, nil, true, borrow, &req); len(warns) > 0 || err != nil || req.Deny != nil {
		t.Errorf("wired codex: %v %v %v", warns, err, req.Deny)
	}
}

// TestAddPolicyOnScreen (T6967): on screen Claude gets the policy as asks and
// no denies; Grok, which has no ask flag, keeps denies; Codex nothing on its
// command line.
func TestAddPolicyOnScreen(t *testing.T) {
	isolate(t)
	plan := parity.PolicyPlan{OnScreen: true}
	var req adapter.InteractiveRequest
	if _, err := addPolicy(adapter.Claude, []string{probe.CapSettings}, true, plan, &req); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"Bash(rm -rf*)", "Bash(git push * main)", "Bash(git push --force*)", "Bash(git reset --hard*)", "Bash(git clean *)", "Read(//**/.env)", "Read(//**/*.pem)", "Read(//**/*.key)", "Bash(git push --force*main*)"} {
		if !slices.Contains(req.Ask, r) {
			t.Errorf("claude ask lacks %s: %v", r, req.Ask)
		}
	}
	if len(req.Deny) != 0 {
		t.Errorf("claude on screen still denies: %v", req.Deny)
	}
	req = adapter.InteractiveRequest{}
	if _, err := addPolicy(adapter.Grok, []string{probe.CapDeny}, true, plan, &req); err != nil {
		t.Fatal(err)
	}
	if len(req.Ask) != 0 || !slices.Contains(req.Deny, "Bash(rm -rf*)") || !slices.Contains(req.Deny, "Read(**/.env)") {
		t.Errorf("grok: ask %v deny %v", req.Ask, req.Deny)
	}
}
