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
		deny bool
	}{
		{adapter.Claude, []string{probe.CapSettings}, true},
		{adapter.Claude, nil, false},
		{adapter.Grok, []string{probe.CapDeny}, true},
		{adapter.Grok, []string{probe.CapRules}, false},
		{adapter.Codex, []string{probe.CapConfigOverride}, false},
	}
	for _, tt := range tests {
		var req adapter.InteractiveRequest
		warns := addPolicy(tt.v, tt.caps, &req)
		want := map[adapter.Vendor][]string{adapter.Claude: parity.ClaudeDeny, adapter.Grok: parity.GrokDeny}[tt.v]
		if got := len(req.Deny) > 0 && slices.Equal(req.Deny, want); got != tt.deny || (len(warns) == 0) != tt.deny {
			t.Errorf("%s %v: deny %v, warns %v", tt.v, tt.caps, req.Deny, warns)
		}
	}
	// Codex warns until the rules file is wired, then needs nothing on
	// its command line.
	mustRun(t, "policy", "wire")
	var req adapter.InteractiveRequest
	if warns := addPolicy(adapter.Codex, nil, &req); len(warns) > 0 || req.Deny != nil {
		t.Errorf("wired codex: %v %v", warns, req.Deny)
	}
}
