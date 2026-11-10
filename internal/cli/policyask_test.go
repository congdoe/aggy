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
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/probe"
)

// policyHere is what a launch of v in cwd would get from the policy, on screen.
func policyHere(t *testing.T, v adapter.Vendor, caps []string) adapter.InteractiveRequest {
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
	if _, err := addPolicy(v, caps, true, policyPlan(planOnScreen, cwd, s), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func claudePolicyHere(t *testing.T) (ask, deny []string) {
	t.Helper()
	req := policyHere(t, adapter.Claude, []string{probe.CapSettings})
	return req.Ask, req.Deny
}

// policyRendered is every rendering of the policy a launch in cwd gets, on
// screen and headless, for Claude, Grok, and OpenCode (with and without
// --auto). Codex's rules file is fixed text with no input, so nothing can
// change it.
func policyRendered(t *testing.T) map[string][]string {
	t.Helper()
	s, _, _, err := permissionsHere(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if s.FileErr != nil {
		t.Fatal(s.FileErr)
	}
	cwd, _ := os.Getwd()
	out := map[string][]string{}
	for _, f := range []planFor{planOnScreen, planHeadless} {
		name := map[planFor]string{planOnScreen: "on screen ", planHeadless: "headless "}[f]
		ds := policyPlan(f, cwd, s).Decide()
		out[name+"claude ask"], out[name+"claude deny"] = parity.ClaudePolicy(ds)
		out[name+"grok deny"] = parity.GrokPolicy(ds)
		out[name+"opencode ask"], out[name+"opencode deny"] = parity.OpenCodePolicy(ds, false)
		out[name+"opencode --auto ask"], out[name+"opencode --auto deny"] = parity.OpenCodePolicy(ds, true)
		var req adapter.InteractiveRequest
		if _, err := addPolicy(adapter.Claude, []string{probe.CapSettings}, true, policyPlan(f, cwd, s), &req); err != nil {
			t.Fatal(err)
		}
		out[name+"claude req ask"], out[name+"claude req deny"] = req.Ask, req.Deny
	}
	return out
}

func samePolicy(t *testing.T, what string, got, want map[string][]string) {
	t.Helper()
	for k, w := range want {
		if !slices.Equal(got[k], w) {
			t.Errorf("%s changed %s:\n got %v\nwant %v", what, k, got[k], w)
		}
	}
	if len(want["on screen claude ask"]) == 0 || len(want["headless claude deny"]) == 0 {
		t.Fatalf("no baseline: %v", want)
	}
}

// TestPermissionsAddPolicyCovered (T6967, re-review; T6976): aggy permissions
// add takes an allow that matches, names, or covers a policy rule, changes no
// static rendering, and says per CLI what it does now: an exact command or file
// runs without asking on Claude Code and OpenCode (a strict one is explained
// before the question), a wildcard pre-approves nothing, and neither changes
// Codex or Grok Build.
func TestPermissionsAddPolicyCovered(t *testing.T) {
	isolate(t)
	inRepo(t)
	base := policyRendered(t)
	for _, tt := range []struct {
		rule          string
		exact, strict bool
	}{
		{"Bash(git push *)", false, false},
		{"Bash(git push --force-with-lease origin main)", true, true},
		{"Read(./certs/server.pem)", true, true},
		{"Bash(rm -rf dist)", true, false},
		{"Bash(rm:*)", false, false},
	} {
		atKeyboard(t, "y\n")
		code, out, errb := run("permissions", "add", "allow", tt.rule)
		if code != 0 {
			t.Fatalf("add %s: %d %q %q", tt.rule, code, out, errb)
		}
		want := []string{"aggy's safety policy (", "Codex and Grok Build: no effect on policy matches"}
		if tt.exact {
			want = append(want, "pre-approves an exact match", "Claude Code: runs without asking", "Any other spelling still asks")
		} else {
			want = append(want, "only one exact command or file pre-approves", "aggy permissions policy <group> deny")
		}
		if tt.strict {
			want = append(want, "pre-approves a strict safety policy case", "repo trusted for OpenCode without --auto")
		}
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("add %s lacks %q: %q", tt.rule, w, out)
			}
		}
		if !tt.exact && strings.Contains(out, "runs without asking") {
			t.Errorf("add %s claims a pre-approval: %q", tt.rule, out)
		}
		if !tt.strict && strings.Contains(out, "strict") {
			t.Errorf("add %s calls itself strict: %q", tt.rule, out)
		}
		samePolicy(t, "add "+tt.rule, policyRendered(t), base)
	}
	atKeyboard(t, "y\n")
	if code, out, _ := run("permissions", "add", "allow", "Bash(npm test)"); code != 0 || strings.Contains(out, "safety policy") {
		t.Errorf("npm test: %d %q", code, out)
	}
}

// TestAllowsNeverChangePolicy (T6967, re-review HIGH-N1): with allows from
// every source the shared permissions read (aggy's file in both scopes, the
// person's Claude settings, a trusted repo's untracked settings.local.json),
// exact and wildcard, every policy rule renders as with none, on screen and
// headless. The HIGH-N1 case: Bash(rm:*) in ~/.claude/settings.json plus
// aggy permissions add allow 'Bash(rm -rf dist)' left rm -rf ~ unasked.
func TestAllowsNeverChangePolicy(t *testing.T) {
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
	base := policyRendered(t)

	writeAt(t, filepath.Join(os.Getenv("HOME"), ".claude", "settings.json"), `{"permissions":{"allow":["Bash(rm:*)"]}}`, 0o600)
	atKeyboard(t, "y\n")
	if code, out, errb := run("permissions", "add", "allow", "Bash(rm -rf dist)"); code != 0 {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	got := policyRendered(t)
	samePolicy(t, "HIGH-N1", got, base)
	if !slices.Contains(got["on screen claude req ask"], "Bash(rm -rf*)") {
		t.Errorf("rm -rf no longer asks on Claude: %v", got["on screen claude req ask"])
	}

	writeAt(t, filepath.Join(repo, ".claude", "settings.local.json"),
		`{"permissions":{"allow":["Bash(rm -*)","Bash(git p*)","Bash(git push *)","Bash(git clean -n)","Bash(git push --force origin main)","Read(./server.pem)","Read(**)","Bash"]}}`, 0o600)
	samePolicy(t, "settings.local.json", policyRendered(t), base)

	for _, args := range [][]string{
		{"Bash(git reset --hard origin/main)", "--everywhere"},
		{"Bash(git push --force origin main)"},
		{"Read(./.env)", "--everywhere"},
	} {
		atKeyboard(t, "y\n")
		if code, out, errb := run(append([]string{"permissions", "add", "allow"}, args...)...); code != 0 {
			t.Fatalf("add %v: %d %q %q", args, code, out, errb)
		}
	}
	samePolicy(t, "aggy's file", policyRendered(t), base)

	// Grok: the deny stays, and no allow naming a policy rule is passed.
	s, _, _, err := permissionsHere(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var req adapter.InteractiveRequest
	if _, err := addPolicy(adapter.Grok, []string{probe.CapDeny, probe.CapAllow}, true, policyPlan(planOnScreen, repo, s), &req); err != nil {
		t.Fatal(err)
	}
	notes := addPermissions(adapter.Grok, []string{probe.CapDeny, probe.CapAllow}, s, &req)
	cmd, err := grok.New().Interactive(req)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(cmd.Args, " ")
	if strings.Contains(joined, "--allow Bash(rm -rf dist)") || strings.Contains(joined, "--allow Bash(git push *)") || !strings.Contains(joined, "--deny Bash(rm -rf*)") {
		t.Errorf("grok argv: %s", joined)
	}
	if !slices.ContainsFunc(notes, func(n string) bool { return strings.Contains(n, "does not go to Grok") }) {
		t.Errorf("notes: %q", notes)
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
	if ask, deny := claudePolicyHere(t); !slices.Contains(ask, "Bash(rm -rf*)") || slices.Contains(deny, "Bash(rm -rf*)") {
		t.Fatalf("with an allow: ask %v deny %v", ask, deny)
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
	if ask, deny = claudePolicyHere(t); slices.Contains(deny, "Bash(rm -rf*)") || !slices.Contains(ask, "Bash(rm -rf*)") {
		t.Errorf("back to ask: ask %v deny %v", ask, deny)
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

// TestPermissionsShowPolicy (T6976): show says how each CLI gets the policy,
// and what each allow naming a policy rule does: an exact one from aggy's file
// pre-approves once it applies (here, once the repo is trusted), a wildcard
// never does.
func TestPermissionsShowPolicy(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	for _, rule := range []string{"Bash(git push --force origin main)", "Bash(git push *)"} {
		atKeyboard(t, "y\n")
		if code, out, errb := run("permissions", "add", "allow", rule); code != 0 {
			t.Fatalf("add: %d %q %q", code, out, errb)
		}
	}
	code, out, _ := run("permissions", "show")
	for _, w := range []string{"Safety policy, on screen", permissions.PolicyForceMain + ": ask [the default]",
		"your allow Bash(git push --force origin main) [aggy, this repo] would pre-approve this exact command, but it does not apply here",
		"your allow Bash(git push *) [aggy, this repo] names or covers this group and pre-approves nothing",
		"asks made by aggy's decision hook", "Grok has no flag for an ask rule", "an allow that names one is not passed to Grok", "Codex: its rules file asks",
		"file reads stay refused by aggy's guard", "no pre-approval changes it"} {
		if code != 0 || !strings.Contains(out, w) {
			t.Errorf("show lacks %q: %d\n%s", w, code, out)
		}
	}
	atKeyboard(t, "y\n")
	if code, out, errb := run("trust", repo); code != 0 {
		t.Fatalf("trust: %d %q %q", code, out, errb)
	}
	code, out, _ = run("permissions", "show")
	if w := "your allow Bash(git push --force origin main) [aggy, this repo] pre-approves this exact command or file"; code != 0 || !strings.Contains(out, w) {
		t.Errorf("trusted, show lacks %q:\n%s", w, out)
	}
	code, out, _ = run("permissions", "list")
	if code != 0 || !strings.Contains(out, "Exact pre-approvals of the safety policy") ||
		!strings.Contains(out, "Bash(git push --force origin main)  [aggy, this repo] push-main, force-push, force-push-main  STRICT") ||
		strings.Contains(out, "Bash(git push *)  [aggy, this repo] push") {
		t.Errorf("list:\n%s", out)
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
	if !slices.ContainsFunc(parity.CoveredRules("Bash(git push --force origin main)", nil), func(r parity.PolicyRule) bool { return r.Strict }) {
		t.Error("the command does name a strict rule")
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
