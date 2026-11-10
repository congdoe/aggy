package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/probe"
)

// policyHere is what a launch of v in cwd would get from the policy, on screen
// with the person's own flags extra.
func policyHere(t *testing.T, v adapter.Vendor, caps, extra []string) adapter.InteractiveRequest {
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
	if _, err := addPolicy(v, caps, true, policyPlan(planOnScreen, cwd, s, extra), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func claudePolicyHere(t *testing.T) (ask, deny []string) {
	t.Helper()
	req := policyHere(t, adapter.Claude, []string{probe.CapSettings}, nil)
	return req.Ask, req.Deny
}

// TestPermissionsAddPreApproves (T6967 b, c, review 4 and 10): an allow added
// with aggy permissions add removes, on Claude only, the ordinary Bash patterns
// it matches, and says where it takes effect and where not. A strict or read
// rule it names stays, and the line says so.
func TestPermissionsAddPreApproves(t *testing.T) {
	isolate(t)
	inRepo(t)
	const forceMain = "Bash(git push --force*main*)"

	atKeyboard(t, "y\n")
	code, out, errb := run("permissions", "add", "allow", "Bash(git push *)")
	if code != 0 {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	if !strings.Contains(out, "On Claude Code this pre-approves") || !strings.Contains(out, "Grok Build, Codex, and OpenCode still refuse or ask") {
		t.Errorf("broad allow: %q", out)
	}
	ask, _ := claudePolicyHere(t)
	if slices.Contains(ask, "Bash(git push * main)") || !slices.Contains(ask, forceMain) {
		t.Errorf("broad allow: ask %v", ask)
	}

	atKeyboard(t, "y\n")
	code, out, errb = run("permissions", "add", "allow", "Bash(git push --force-with-lease origin main)")
	if code != 0 {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	if n := strings.Count(out, "pre-approves"); n != 1 || !strings.Contains(out, "does not change the safety policy's "+permissions.PolicyForceMain) {
		t.Errorf("naming allow: %q", out)
	}
	ask, _ = claudePolicyHere(t)
	for _, r := range []string{forceMain, "Bash(git push * --force*main*)", "Bash(git push --force*master*)"} {
		if !slices.Contains(ask, r) {
			t.Errorf("a strict rule lost its ask: %s", r)
		}
	}

	// A key file read changes nothing anywhere, and says so.
	atKeyboard(t, "y\n")
	if code, out, errb = run("permissions", "add", "allow", "Read(./certs/server.pem)"); code != 0 || !strings.Contains(out, "does not change the safety policy's "+permissions.PolicyKeys) || strings.Contains(out, "On Claude Code") {
		t.Fatalf("key read: %d %q %q", code, out, errb)
	}
	if ask, _ = claudePolicyHere(t); !slices.Contains(ask, "Read(//**/*.pem)") {
		t.Errorf("key read: ask %v", ask)
	}

	// An allow that names nothing of the policy says nothing about it.
	atKeyboard(t, "y\n")
	if code, out, _ = run("permissions", "add", "allow", "Bash(npm test)"); code != 0 || strings.Contains(out, "pre-approves") || strings.Contains(out, "safety policy") {
		t.Errorf("npm test: %d %q", code, out)
	}
}

// TestPreApprovalOnlyOnClaude (review 1, 2): Grok keeps the deny and gets no
// allow that names a policy rule; under a Claude mode with no prompt, no
// pre-approval takes effect.
func TestPreApprovalOnlyOnClaude(t *testing.T) {
	isolate(t)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q")
	t.Chdir(repo)
	atKeyboard(t, "y\n")
	if code, out, errb := run("trust", repo); code != 0 {
		t.Fatalf("trust: %d %q %q", code, out, errb)
	}
	atKeyboard(t, "y\n")
	if code, out, errb := run("permissions", "add", "allow", "Bash(rm -rf dist)", "--everywhere"); code != 0 {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	if ask, _ := claudePolicyHere(t); slices.Contains(ask, "Bash(rm -rf*)") {
		t.Errorf("claude still asks: %v", ask)
	}
	req := policyHere(t, adapter.Claude, []string{probe.CapSettings}, []string{"--permission-mode", "acceptEdits"})
	if !slices.Contains(req.Ask, "Bash(rm -rf*)") {
		t.Errorf("acceptEdits: the rule was removed with no prompt behind it: %v", req.Ask)
	}
	req = policyHere(t, adapter.Grok, []string{probe.CapDeny, probe.CapAllow}, nil)
	if !slices.Contains(req.Deny, "Bash(rm -rf*)") {
		t.Errorf("grok lost the deny: %v", req.Deny)
	}
	s, _, _, err := permissionsHere(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	notes := addPermissions(adapter.Grok, []string{probe.CapDeny, probe.CapAllow}, s, &req)
	cmd, err := grok.New().Interactive(req)
	if err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(cmd.Args, " "); strings.Contains(joined, "--allow Bash(rm -rf dist)") || !strings.Contains(joined, "--deny Bash(rm -rf*)") {
		t.Errorf("grok argv: %s", joined)
	}
	if !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "does not go to Grok") }) {
		t.Errorf("notes: %q", notes)
	}
}

// TestPolicyNotGrantedByRepo (T6967 b, c, review 8): an allow in the repo's
// settings.local.json never removes a strict or read rule, a tracked one
// removes nothing, and an untracked one in a trusted repo removes only the
// ordinary Bash patterns it names, never from a wide wildcard.
func TestPolicyNotGrantedByRepo(t *testing.T) {
	isolate(t)
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q")
	t.Chdir(repo)
	local := filepath.Join(repo, ".claude", "settings.local.json")
	writeAt(t, local, `{"permissions":{"allow":["Bash(git push --force origin main)","Read(./server.pem)","Bash(git clean -n)","Bash(rm:*)"]}}`, 0o600)
	gitIn(t, repo, "add", "-f", ".claude/settings.local.json")
	ask, _ := claudePolicyHere(t)
	for _, r := range []string{"Bash(git push --force*main*)", "Read(//**/*.pem)", "Bash(git clean *)", "Bash(git push --force*)", "Bash(rm -rf*)"} {
		if !slices.Contains(ask, r) {
			t.Errorf("a committed file pre-approved %s", r)
		}
	}
	gitIn(t, repo, "rm", "-q", "--cached", ".claude/settings.local.json")
	atKeyboard(t, "y\n")
	if code, out, errb := run("trust", repo); code != 0 {
		t.Fatalf("trust: %d %q %q", code, out, errb)
	}
	ask, _ = claudePolicyHere(t)
	if slices.Contains(ask, "Bash(git clean *)") || slices.Contains(ask, "Bash(git push --force*)") {
		t.Errorf("the person's own prompt answers did not carry: %v", ask)
	}
	for _, r := range []string{"Bash(git push --force*main*)", "Read(//**/*.pem)", "Bash(rm -rf*)", "Bash(rm -fr*)"} {
		if !slices.Contains(ask, r) {
			t.Errorf("a CLI's file removed %s", r)
		}
	}
}

// TestPermissionsPolicyMove (T6967 e): a group moves to deny and back, and a
// deny wins over an allow that names it.
func TestPermissionsPolicyMove(t *testing.T) {
	isolate(t)
	inRepo(t)
	atKeyboard(t, "y\n")
	if code, out, errb := run("permissions", "add", "allow", "Bash(rm -rf dist)"); code != 0 {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	if ask, deny := claudePolicyHere(t); slices.Contains(ask, "Bash(rm -rf*)") || slices.Contains(deny, "Bash(rm -rf*)") {
		t.Fatalf("pre-approved: ask %v deny %v", ask, deny)
	}
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
	path, err := permissionsFile()
	if err != nil {
		t.Fatal(err)
	}
	writeAt(t, path, `{"policy":{"rm-rf":"allow"}}`, 0o600)
	if s, _, _, _ := permissionsHere(t.Context()); s.FileErr == nil {
		t.Error("a policy group set to allow was taken")
	}
}

// TestPolicyTwoRepoKeys (review 10): two keys naming one repo, the stricter
// setting wins.
func TestPolicyTwoRepoKeys(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	path, err := permissionsFile()
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range [][2]string{{repo, link}, {link, repo}} {
		body, _ := json.Marshal(map[string]any{"repos": map[string]any{
			order[0]: map[string]any{"policy": map[string]string{permissions.PolicyRmRf: "deny"}},
			order[1]: map[string]any{"policy": map[string]string{permissions.PolicyRmRf: "ask"}},
		}})
		writeAt(t, path, string(body), 0o600)
		if _, deny := claudePolicyHere(t); !slices.Contains(deny, "Bash(rm -rf*)") {
			t.Errorf("%v: the ask won", order)
		}
	}
}

// TestPermissionsShowPolicy: show says how each CLI gets the policy, and that
// a pre-approval takes effect on Claude Code only.
func TestPermissionsShowPolicy(t *testing.T) {
	isolate(t)
	inRepo(t)
	atKeyboard(t, "y\n")
	if code, out, errb := run("permissions", "add", "allow", "Bash(git push --force origin main)"); code != 0 {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	code, out, _ := run("permissions", "show")
	for _, w := range []string{"Safety policy, on screen", permissions.PolicyForceMain + ": ask [the default]", "pre-approved on Claude Code only",
		"not pre-approved: Bash(git push --force*main*)", "Grok has no flag for an ask rule", "a pre-approval does not change them", "Codex: its rules file asks", "file reads stay refused by aggy's guard"} {
		if code != 0 || !strings.Contains(out, w) {
			t.Errorf("show lacks %q: %d\n%s", w, code, out)
		}
	}
}

// TestSuggestNeverStrict: suggest never proposes a command that names a strict
// policy rule.
func TestSuggestNeverStrict(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	autoSession(t, repo, "a.jsonl", map[string]int{"git push --force origin main": 4, "gh pr view": 3})
	atKeyboard(t, "n\n")
	code, out, errb := run("permissions", "suggest")
	if code != 0 || strings.Contains(out, "git push") || !strings.Contains(out, "gh pr view") {
		t.Errorf("%d %q %q", code, out, errb)
	}
}

// TestGuardEnvTemplates (review 7): the OpenCode guard lets the four
// templates through and refuses an unusual .env name.
func TestGuardEnvTemplates(t *testing.T) {
	isolate(t)
	repo := tempRepo(t)
	for _, tc := range []struct {
		name string
		code int
	}{{".env.example", 0}, {".env.dist", 0}, {".env.vault", 2}, {".env", 2}} {
		writeAt(t, filepath.Join(repo, tc.name), "x", 0o600)
		b, err := json.Marshal(hook.OpenCodeInput{Tool: "read", Directory: repo, Root: repo, Path: tc.name})
		if err != nil {
			t.Fatal(err)
		}
		var out, errb bytes.Buffer
		if got := hookWith([]string{"opencode-guard"}, bytes.NewReader(b), func(string) string { return "" }, &out, &errb); (got == 0) != (tc.code == 0) {
			t.Errorf("%s: %d %q %q", tc.name, got, out.String(), errb.String())
		}
	}
}
