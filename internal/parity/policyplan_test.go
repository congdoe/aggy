package parity

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/permissions"
)

// readGlob matches a file name against a Read rule's name, where * is any
// text: how Claude, Grok, and the OpenCode guard read these names.
func readGlob(rule, name string) bool {
	return glob(path.Base(rule[:len(rule)-1]), name)
}

// TestEnvTemplates (T6967 d): templates meant to be read are not guarded;
// the usual secret-bearing names are.
func TestEnvTemplates(t *testing.T) {
	guarded := func(name string) bool {
		for _, r := range ClaudeDeny {
			if len(r) > 5 && r[:5] == "Read(" && readGlob(r, name) {
				return true
			}
		}
		return false
	}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{".env", true}, {".env.local", true}, {".env.production", true}, {".env.development.local", true},
		{"server.pem", true}, {"tls.key", true}, {"id_ed25519", true},
		{".env.example", false}, {".env.sample", false}, {".env.template", false}, {".env.dist", false},
		{"README.md", false},
	} {
		if got := guarded(tc.name); got != tc.want {
			t.Errorf("%s guarded = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func ruleSet(rules ...permissions.Rule) []permissions.Rule { return rules }

func allow(text string, o permissions.Origin) permissions.Rule {
	return permissions.Rule{Kind: permissions.Allow, Text: text, Origin: o}
}

func kindOf(ds []PolicyDecision, text string) permissions.Kind {
	for _, d := range ds {
		if d.Text == text {
			return d.Kind
		}
	}
	return ""
}

// TestPolicyPlanBorrow: a borrow child gets exactly the deny list it had.
func TestPolicyPlanBorrow(t *testing.T) {
	plan := PolicyPlan{Allows: ruleSet(allow("Bash(rm -rf dist)", permissions.FromAggy)), Branches: []string{"trunk"}}
	ask, deny := ClaudePolicy(plan.Decide())
	if len(ask) != 0 || !slices.Equal(deny, ClaudeDeny) {
		t.Errorf("claude borrow: ask %v, deny %v", ask, deny)
	}
	if got := GrokPolicy(plan.Decide()); !slices.Equal(got, GrokDeny) {
		t.Errorf("grok borrow: %v", got)
	}
}

// TestPolicyPlanOnScreen: every rule asks, and the pre-approval rules hold.
func TestPolicyPlanOnScreen(t *testing.T) {
	const (
		rmRf       = "Bash(rm -rf*)"
		pushMain   = "Bash(git push * main)"
		force      = "Bash(git push --force*)"
		forceMain  = "Bash(git push --force*main*)"
		forceTrunk = "Bash(git push --force*trunk*)"
		pem        = "Read(//**/*.pem)"
		env        = "Read(//**/.env)"
	)
	tests := []struct {
		name     string
		allows   []permissions.Rule
		settings map[string]permissions.Kind
		want     map[string]permissions.Kind
	}{
		{"nothing", nil, nil, map[string]permissions.Kind{rmRf: permissions.Ask, pushMain: permissions.Ask, force: permissions.Ask, forceMain: permissions.Ask, forceTrunk: permissions.Ask, pem: permissions.Ask, env: permissions.Ask}},
		{"a naming allow from the prompt", ruleSet(allow("Bash(rm -rf dist)", permissions.FromClaudeLocal)), nil,
			map[string]permissions.Kind{rmRf: permissions.Allow, "Bash(rm -fr*)": permissions.Ask}},
		{"a broad allow covers ordinary rules, not strict ones", ruleSet(allow("Bash(git push *)", permissions.FromAggy)), nil,
			map[string]permissions.Kind{pushMain: permissions.Allow, force: permissions.Allow, forceMain: permissions.Ask, forceTrunk: permissions.Ask}},
		{"a broad read does not cover keys", ruleSet(allow("Read(**)", permissions.FromAggy)), nil,
			map[string]permissions.Kind{env: permissions.Allow, pem: permissions.Ask}},
		{"a strict allow at a CLI's prompt is not carried", ruleSet(allow("Bash(git push --force origin main)", permissions.FromClaudeLocal), allow("Read(./certs/server.pem)", permissions.FromGrokGrant)), nil,
			map[string]permissions.Kind{force: permissions.Allow, forceMain: permissions.Ask, pem: permissions.Ask}},
		{"a naming allow in aggy's file pre-approves strict", ruleSet(allow("Bash(git push --force origin main)", permissions.FromAggyRepo), allow("Read(./certs/server.pem)", permissions.FromAggy)), nil,
			map[string]permissions.Kind{forceMain: permissions.Allow, forceTrunk: permissions.Ask, pem: permissions.Allow, "Read(//**/*.key)": permissions.Ask}},
		{"a force push to another branch is ordinary", ruleSet(allow("Bash(git push --force origin feature)", permissions.FromClaudeLocal)), nil,
			map[string]permissions.Kind{force: permissions.Allow, forceMain: permissions.Ask}},
		{"a deny setting wins over an allow", ruleSet(allow("Bash(rm -rf dist)", permissions.FromAggy)),
			map[string]permissions.Kind{permissions.PolicyRmRf: permissions.Deny},
			map[string]permissions.Kind{rmRf: permissions.Deny, pushMain: permissions.Ask}},
	}
	for _, tt := range tests {
		ds := PolicyPlan{OnScreen: true, Allows: tt.allows, Settings: tt.settings, Branches: []string{"trunk"}}.Decide()
		for text, want := range tt.want {
			if got := kindOf(ds, text); got != want {
				t.Errorf("%s: %s = %q, want %q", tt.name, text, got, want)
			}
		}
	}
}

// TestPolicyRenderings: what each CLI can express.
func TestPolicyRenderings(t *testing.T) {
	ds := PolicyPlan{OnScreen: true, Allows: ruleSet(allow("Bash(git clean -n)", permissions.FromAggy))}.Decide()
	ask, deny := ClaudePolicy(ds)
	if len(deny) != 0 || !slices.Contains(ask, "Bash(rm -rf*)") || slices.Contains(ask, "Bash(git clean *)") {
		t.Errorf("claude: ask %v deny %v", ask, deny)
	}
	// Grok has no ask flag: the asks stay denials, never allows, and a
	// pre-approved rule is left out.
	g := GrokPolicy(ds)
	if !slices.Contains(g, "Bash(rm -rf*)") || !slices.Contains(g, "Read(**/.env)") || slices.Contains(g, "Bash(git clean *)") {
		t.Errorf("grok: %v", g)
	}
	oAsk, oDeny := OpenCodePolicy(ds, false)
	if !slices.Contains(oAsk, "rm -rf*") || len(oDeny) != 0 {
		t.Errorf("opencode: ask %v deny %v", oAsk, oDeny)
	}
	// --auto answers an ask, so there every ask is a deny.
	oAsk, oDeny = OpenCodePolicy(ds, true)
	if len(oAsk) != 0 || !slices.Contains(oDeny, "shell:rm -rf*") {
		t.Errorf("opencode --auto: ask %v deny %v", oAsk, oDeny)
	}
}

// TestPreApproves: what aggy permissions add names, and what suggest skips.
func TestPreApproves(t *testing.T) {
	strict := func(rs []PolicyRule) bool {
		return slices.ContainsFunc(rs, func(r PolicyRule) bool { return r.Strict })
	}
	if rs := PreApproves("Bash(git push --force origin main)", nil); !strict(rs) {
		t.Errorf("naming force push to main: %v", rs)
	}
	if rs := PreApproves("Bash(git push *)", nil); strict(rs) || len(rs) == 0 {
		t.Errorf("broad push: %v", rs)
	}
	if rs := PreApproves("Bash(npm test)", nil); len(rs) != 0 {
		t.Errorf("npm test: %v", rs)
	}
	if rs := PreApproves("Bash(git push --force origin trunk)", []string{"trunk"}); !strict(rs) {
		t.Errorf("default branch: %v", rs)
	}
}

// TestDefaultBranch reads origin/HEAD, and refuses a name that is not plain.
func TestDefaultBranch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	git := filepath.Join(root, ".git")
	for _, d := range []string{"objects", "refs/remotes/origin"} {
		if err := os.MkdirAll(filepath.Join(git, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(git, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b := DefaultBranch(root); b != "" {
		t.Errorf("no origin/HEAD: %q", b)
	}
	head := filepath.Join(git, "refs/remotes/origin/HEAD")
	for content, want := range map[string]string{
		"ref: refs/remotes/origin/trunk\n": "trunk",
		"ref: refs/remotes/origin/a*b\n":   "",
		"ref: refs/remotes/origin/x,y\n":   "",
		"0123abcd\n":                       "",
	} {
		if err := os.WriteFile(head, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if b := DefaultBranch(root); b != want {
			t.Errorf("%q: %q, want %q", content, b, want)
		}
	}
}

// TestOpenCodePolicyRendered (T6967): on screen OpenCode's config asks before
// the policy's shell rules, after every allow so the ask wins; under --auto,
// which answers an ask, they stay denies; key file reads stay denied.
func TestOpenCodePolicyRendered(t *testing.T) {
	has := func(rs []openCodeRule, want openCodeRule) bool { return slices.Contains(rs, want) }
	c := baseConfig()
	c.Policy = PolicyPlan{OnScreen: true}.Decide()
	d := renderDoc(t, c)
	last := d.Permissions[len(d.Permissions)-1]
	if last.Effect != "ask" || !has(d.Permissions, openCodeRule{"shell", "rm -rf*", "ask"}) {
		t.Errorf("on screen, permissions end %+v", last)
	}
	if has(d.Policies, openCodeRule{"tool.use", "shell:rm -rf*", "deny"}) || !has(d.Policies, openCodeRule{"tool.use", "read:*.pem", "deny"}) {
		t.Errorf("on screen, policies %+v", d.Policies)
	}
	c.Auto = true
	d = renderDoc(t, c)
	if has(d.Permissions, openCodeRule{"shell", "rm -rf*", "ask"}) || !has(d.Policies, openCodeRule{"tool.use", "shell:rm -rf*", "deny"}) {
		t.Errorf("--auto: permissions %+v policies %+v", d.Permissions, d.Policies)
	}
	// With no decisions, every rule is a deny, as before.
	d = renderDoc(t, baseConfig())
	if !has(d.Policies, openCodeRule{"tool.use", "shell:rm -rf*", "deny"}) {
		t.Errorf("no decisions: %+v", d.Policies)
	}
}
