package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/probe"
)

// claudePolicyHere is what a Claude launch in cwd would get from the policy.
func claudePolicyHere(t *testing.T) (ask, deny []string) {
	t.Helper()
	s, _, _, err := permissionsHere(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if s.FileErr != nil {
		t.Fatal(s.FileErr)
	}
	cwd, _ := os.Getwd()
	var req adapter.InteractiveRequest
	if _, err := addPolicy(adapter.Claude, []string{probe.CapSettings}, true, policyPlan(true, cwd, s), &req); err != nil {
		t.Fatal(err)
	}
	return req.Ask, req.Deny
}

// TestPermissionsAddPreApproves (T6967 b, c): an allow added with aggy
// permissions add pre-approves the policy rules it names, and says so on one
// line; a broad one does not reach the strict rules.
func TestPermissionsAddPreApproves(t *testing.T) {
	isolate(t)
	inRepo(t)
	const forceMain = "Bash(git push --force*main*)"

	atKeyboard(t, "y\n")
	code, out, errb := run("permissions", "add", "allow", "Bash(git push *)")
	if code != 0 {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	if !strings.Contains(out, "pre-approves") || strings.Contains(out, "strict") {
		t.Errorf("broad allow: %q", out)
	}
	ask, _ := claudePolicyHere(t)
	if slices.Contains(ask, "Bash(git push * main)") || !slices.Contains(ask, forceMain) {
		t.Errorf("broad allow: ask %v", ask)
	}

	atKeyboard(t, "y\n")
	code, out, errb = run("permissions", "add", "allow", "Bash(git push --force origin main)")
	if code != 0 {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	if n := strings.Count(out, "pre-approves"); n != 1 || !strings.Contains(out, permissions.PolicyForceMain) || !strings.Contains(out, "strict") {
		t.Errorf("naming allow: %q", out)
	}
	if ask, _ = claudePolicyHere(t); slices.Contains(ask, forceMain) {
		t.Errorf("naming allow: still asks %v", ask)
	}

	// A key file read, by a path whose name the rule names.
	atKeyboard(t, "y\n")
	if code, out, errb = run("permissions", "add", "allow", "Read(./certs/server.pem)"); code != 0 || !strings.Contains(out, permissions.PolicyKeys) {
		t.Fatalf("key read: %d %q %q", code, out, errb)
	}
	if ask, _ = claudePolicyHere(t); slices.Contains(ask, "Read(//**/*.pem)") || !slices.Contains(ask, "Read(//**/*.key)") {
		t.Errorf("key read: ask %v", ask)
	}

	// An allow that names nothing of the policy says nothing about it.
	atKeyboard(t, "y\n")
	if code, out, _ = run("permissions", "add", "allow", "Bash(npm test)"); code != 0 || strings.Contains(out, "pre-approves") {
		t.Errorf("npm test: %d %q", code, out)
	}
}

// TestPolicyNotGrantedByRepo (T6967 b, c): an allow in the repo's
// settings.local.json, whether git tracks it or not, never pre-approves a
// strict rule, and a tracked one pre-approves nothing.
func TestPolicyNotGrantedByRepo(t *testing.T) {
	isolate(t)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q")
	t.Chdir(repo)
	local := filepath.Join(repo, ".claude", "settings.local.json")
	writeAt(t, local, `{"permissions":{"allow":["Bash(git push --force origin main)","Read(./server.pem)","Bash(git clean -n)"]}}`, 0o600)
	gitIn(t, repo, "add", "-f", ".claude/settings.local.json")
	ask, _ := claudePolicyHere(t)
	for _, r := range []string{"Bash(git push --force*main*)", "Read(//**/*.pem)", "Bash(git clean *)", "Bash(git push --force*)"} {
		if !slices.Contains(ask, r) {
			t.Errorf("a committed file pre-approved %s", r)
		}
	}
	// Untracked, and the repo trusted: it is the person's, so it pre-approves
	// the ordinary rules it names, never the strict ones.
	gitIn(t, repo, "rm", "-q", "--cached", ".claude/settings.local.json")
	atKeyboard(t, "y\n")
	if code, out, errb := run("trust", repo); code != 0 {
		t.Fatalf("trust: %d %q %q", code, out, errb)
	}
	ask, _ = claudePolicyHere(t)
	if slices.Contains(ask, "Bash(git clean *)") || slices.Contains(ask, "Bash(git push --force*)") {
		t.Errorf("the person's own prompt answers did not carry: %v", ask)
	}
	if !slices.Contains(ask, "Bash(git push --force*main*)") || !slices.Contains(ask, "Read(//**/*.pem)") {
		t.Errorf("a prompt answer pre-approved a strict rule: %v", ask)
	}
}

// TestPermissionsPolicyMove (T6967 e): a group moves to deny and back, and a
// deny wins over an allow that names it.
func TestPermissionsPolicyMove(t *testing.T) {
	isolate(t)
	inRepo(t)
	writePermissionsAllow := func() {
		atKeyboard(t, "y\n")
		if code, out, errb := run("permissions", "add", "allow", "Bash(rm -rf dist)"); code != 0 {
			t.Fatalf("add: %d %q %q", code, out, errb)
		}
	}
	writePermissionsAllow()
	if ask, deny := claudePolicyHere(t); slices.Contains(ask, "Bash(rm -rf*)") || slices.Contains(deny, "Bash(rm -rf*)") {
		t.Fatalf("pre-approved: ask %v deny %v", ask, deny)
	}
	// No terminal: refused.
	atKeyboard(t, "")
	t.Setenv("AGGY_VENDOR", "claude")
	if code, _, errb := run("permissions", "policy", permissions.PolicyRmRf, "deny"); code != 1 || !strings.Contains(errb, "not_at_keyboard") {
		t.Errorf("from a session's shell: %d %q", code, errb)
	}
	t.Setenv("AGGY_VENDOR", "") // empty is unset to personGate
	for _, args := range [][]string{{"nope", "deny"}, {permissions.PolicyRmRf, "allow"}} {
		if code, _, errb := run(append([]string{"permissions", "policy"}, args...)...); code != 1 || !strings.Contains(errb, "permissions.bad_rule") {
			t.Errorf("%q: %d %q", args, code, errb)
		}
	}
	atKeyboard(t, "y\n")
	if code, out, errb := run("permissions", "policy", permissions.PolicyRmRf, "deny"); code != 0 {
		t.Fatalf("to deny: %d %q %q", code, out, errb)
	}
	ask, deny := claudePolicyHere(t)
	if slices.Contains(ask, "Bash(rm -rf*)") || !slices.Contains(deny, "Bash(rm -rf*)") || !slices.Contains(ask, "Bash(git clean *)") {
		t.Errorf("deny wins over the allow: ask %v deny %v", ask, deny)
	}
	if code, out, _ := run("permissions", "policy"); code != 0 || !strings.Contains(out, permissions.PolicyRmRf+": deny [aggy, this repo]") {
		t.Errorf("list: %d %q", code, out)
	}
	atKeyboard(t, "y\n")
	if code, out, errb := run("permissions", "policy", permissions.PolicyRmRf, "ask"); code != 0 {
		t.Fatalf("back to ask: %d %q %q", code, out, errb)
	}
	if ask, deny = claudePolicyHere(t); slices.Contains(deny, "Bash(rm -rf*)") || slices.Contains(ask, "Bash(rm -rf*)") {
		t.Errorf("back to ask, the allow pre-approves again: ask %v deny %v", ask, deny)
	}
	// A hand-edited setting aggy does not know refuses a launch.
	path, err := permissionsFile()
	if err != nil {
		t.Fatal(err)
	}
	writeAt(t, path, `{"policy":{"rm-rf":"allow"}}`, 0o600)
	if s, _, _, _ := permissionsHere(t.Context()); s.FileErr == nil {
		t.Error("a policy group set to allow was taken")
	}
}

// TestPermissionsShowPolicy: show says how each CLI gets the policy, and that
// Grok and Codex cannot take an ask the way Claude does.
func TestPermissionsShowPolicy(t *testing.T) {
	isolate(t)
	inRepo(t)
	code, out, _ := run("permissions", "show")
	for _, w := range []string{"Safety policy, on screen", permissions.PolicyForceMain + ": ask [the default]", "Grok has no flag for an ask rule", "stay denials", "Codex: its rules file asks", "file reads stay refused by aggy's guard"} {
		if code != 0 || !strings.Contains(out, w) {
			t.Errorf("show lacks %q: %d\n%s", w, code, out)
		}
	}
}

// TestSuggestNeverStrict: suggest never proposes a command that would
// pre-approve a strict policy rule.
func TestSuggestNeverStrict(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	autoSession(t, repo, "a.jsonl", map[string]int{"git push --force origin main": 4, "gh pr view": 3})
	atKeyboard(t, "n\n")
	code, out, errb := run("permissions", "suggest")
	if code != 0 || strings.Contains(out, "git push") || !strings.Contains(out, "gh pr view") {
		t.Errorf("%d %q %q", code, out, errb)
	}
	if rs := parity.PreApproves("Bash(git push --force origin main)", nil); len(rs) == 0 {
		t.Error("the command does name a strict rule")
	}
}
