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

func guardedBy(rules []string, name string) bool {
	for _, r := range rules {
		if len(r) > 5 && r[:5] == "Read(" && readGlob(r, name) {
			return true
		}
	}
	return false
}

// TestEnvNames (T6967 d, review 7): on screen the templates are left alone and
// the usual secret-bearing names are guarded; a headless deny keeps .env.*,
// so an unusual name stays refused there.
func TestEnvNames(t *testing.T) {
	onScreen, _ := ClaudePolicy(PolicyPlan{OnScreen: true}.Decide())
	for _, tc := range []struct {
		name               string
		onScreen, headless bool
	}{
		{".env", true, true}, {".env.local", true, true}, {".env.production", true, true},
		{".env.development.local", true, true}, {".env.local.backup", true, true},
		{".env.keys", true, true}, {".env.me", true, true}, {".env.vault", true, true},
		{".env.docker", true, true}, {".env.prd", true, true}, {".env.stg", true, true},
		{".env.override", true, true}, {".env.e2e", true, true},
		{"server.pem", true, true}, {"tls.key", true, true}, {"id_ed25519", true, true},
		{".env.example", false, true}, {".env.sample", false, true}, {".env.template", false, true}, {".env.dist", false, true},
		{".env.foo", false, true}, // not covered on screen: design.md says so
		{"README.md", false, false},
	} {
		if got := guardedBy(onScreen, tc.name); got != tc.onScreen {
			t.Errorf("%s on screen = %v, want %v", tc.name, got, tc.onScreen)
		}
		if got := guardedBy(ClaudeDeny, tc.name); got != tc.headless {
			t.Errorf("%s headless = %v, want %v", tc.name, got, tc.headless)
		}
	}
	if !slices.Contains(OpenCodeDeniedNames(), ".env.*") {
		t.Error("the OpenCode guard lost .env.*")
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

// TestPolicyPlanBorrow: a headless plan gets exactly the deny list it had
// before T6967, whatever allows and settings say.
func TestPolicyPlanBorrow(t *testing.T) {
	plan := PolicyPlan{Allows: ruleSet(allow("Bash(rm -rf dist)", permissions.FromAggy)), Branches: []string{"develop"},
		Settings: map[string]permissions.Kind{permissions.PolicyRmRf: permissions.Ask}}
	ask, deny := ClaudePolicy(plan.Decide())
	if len(ask) != 0 || !slices.Equal(deny, ClaudeDeny) {
		t.Errorf("claude borrow: ask %v, deny %v", ask, deny)
	}
	if got := GrokPolicy(plan.Decide()); !slices.Equal(got, GrokDeny) {
		t.Errorf("grok borrow: %v", got)
	}
}

// TestPolicyPlanOnScreen: every rule asks, and a pre-approval removes a rule
// only for an ordinary Bash group, on Claude, where Claude prompts.
func TestPolicyPlanOnScreen(t *testing.T) {
	const (
		rmRf        = "Bash(rm -rf*)"
		rmFr        = "Bash(rm -fr*)"
		pushMain    = "Bash(git push * main)"
		force       = "Bash(git push --force*)"
		forceMain   = "Bash(git push --force*main*)"
		forceDev    = "Bash(git push --force*develop*)"
		forceMast   = "Bash(git push --force*master*)"
		pem         = "Read(//**/*.pem)"
		env         = "Read(//**/.env)"
		ask, allow_ = permissions.Ask, permissions.Allow
	)
	tests := []struct {
		name     string
		allows   []permissions.Rule
		settings map[string]permissions.Kind
		noPrompt bool
		want     map[string]permissions.Kind
	}{
		{"nothing", nil, nil, false, map[string]permissions.Kind{rmRf: ask, pushMain: ask, force: ask, forceMain: ask, forceDev: ask, forceMast: ask, pem: ask, env: ask}},
		{"an exact allow from the prompt removes the pattern it matches", ruleSet(allow("Bash(rm -rf dist)", permissions.FromClaudeLocal)), nil, false,
			map[string]permissions.Kind{rmRf: allow_, rmFr: ask}},
		{"a wide allow from a CLI's file removes nothing", ruleSet(allow("Bash(rm *)", permissions.FromClaudeLocal), allow("Bash(git:*)", permissions.FromClaudeLocal)), nil, false,
			map[string]permissions.Kind{rmRf: ask, rmFr: ask, pushMain: ask}},
		{"a wide allow from aggy's file covers ordinary rules", ruleSet(allow("Bash(git push *)", permissions.FromAggy)), nil, false,
			map[string]permissions.Kind{pushMain: allow_, force: allow_, forceMain: ask}},
		{"a read allow never removes a read rule", ruleSet(allow("Read(**)", permissions.FromAggy), allow("Read(./.env)", permissions.FromAggy)), nil, false,
			map[string]permissions.Kind{env: ask, pem: ask}},
		{"a strict naming allow, even in aggy's file, removes no strict rule", ruleSet(allow("Bash(git push --force-with-lease origin main)", permissions.FromAggyRepo)), nil, false,
			map[string]permissions.Kind{forceMain: ask, "Bash(git push -f*main*)": ask, "Bash(git push * --force*main*)": ask, force: allow_}},
		{"a key read allow changes nothing", ruleSet(allow("Read(./certs/server.pem)", permissions.FromAggy)), nil, false,
			map[string]permissions.Kind{pem: ask}},
		{"a mode with no prompt behind a removed rule", ruleSet(allow("Bash(rm -rf dist)", permissions.FromAggy)), nil, true,
			map[string]permissions.Kind{rmRf: ask}},
		{"a deny setting wins over an allow", ruleSet(allow("Bash(rm -rf dist)", permissions.FromAggy)),
			map[string]permissions.Kind{permissions.PolicyRmRf: permissions.Deny}, false,
			map[string]permissions.Kind{rmRf: permissions.Deny, pushMain: ask}},
	}
	for _, tt := range tests {
		ds := PolicyPlan{OnScreen: true, NoPrompt: tt.noPrompt, Allows: tt.allows, Settings: tt.settings, Branches: []string{"develop"}}.Decide()
		for _, text := range []string{rmRf, rmFr, pushMain, force, forceMain, forceDev, forceMast, pem, env, "Bash(git push -f*main*)", "Bash(git push * --force*main*)"} {
			want, ok := tt.want[text]
			if !ok {
				continue
			}
			if got := kindOf(ds, text); got != want {
				t.Errorf("%s: %s = %q, want %q", tt.name, text, got, want)
			}
		}
	}
}

// TestPolicyUnusedSaid: an allow in aggy's file that names a rule it cannot
// pre-approve is reported with why, for add and show.
func TestPolicyUnusedSaid(t *testing.T) {
	ds := PolicyPlan{OnScreen: true, Allows: ruleSet(allow("Bash(git push --force origin main)", permissions.FromAggy))}.Decide()
	for _, d := range ds {
		if d.Text == "Bash(git push --force*main*)" && (d.Unused.Text == "" || d.Why == "") {
			t.Errorf("strict rule, unused not said: %+v", d)
		}
	}
}

// TestPolicyRenderingsWithPreApproval (review 1, 2, 6): a pre-approval
// removes a rule from Claude's rendering only. Grok keeps the deny, OpenCode
// keeps the ask (a deny under --auto), and OpenCode's strict rules are denies.
func TestPolicyRenderingsWithPreApproval(t *testing.T) {
	ds := PolicyPlan{OnScreen: true, Allows: ruleSet(allow("Bash(rm -rf dist)", permissions.FromAggy), allow("Bash(git reset --hard origin/main)", permissions.FromAggy))}.Decide()
	ask, deny := ClaudePolicy(ds)
	if len(deny) != 0 || slices.Contains(ask, "Bash(rm -rf*)") || slices.Contains(ask, "Bash(git reset --hard*)") || !slices.Contains(ask, "Bash(rm -fr*)") {
		t.Errorf("claude: ask %v deny %v", ask, deny)
	}
	g := GrokPolicy(ds)
	for _, r := range []string{"Bash(rm -rf*)", "Bash(git reset --hard*)", "Read(**/.env)", "Bash(git push --force*main*)"} {
		if !slices.Contains(g, r) {
			t.Errorf("grok lacks the deny %s: %v", r, g)
		}
	}
	oAsk, oDeny := OpenCodePolicy(ds, false)
	if !slices.Contains(oAsk, "rm -rf*") || !slices.Contains(oAsk, "git reset --hard*") || !slices.Contains(oDeny, "shell:git push --force*main*") || slices.Contains(oAsk, "git push --force*main*") {
		t.Errorf("opencode: ask %v deny %v", oAsk, oDeny)
	}
	// --auto answers an ask, so there every rule, pre-approved or not, is a deny.
	oAsk, oDeny = OpenCodePolicy(ds, true)
	if len(oAsk) != 0 || !slices.Contains(oDeny, "shell:rm -rf*") || !slices.Contains(oDeny, "shell:git reset --hard*") {
		t.Errorf("opencode --auto: ask %v deny %v", oAsk, oDeny)
	}
}

// TestPreApproves: what suggest checks for a strict rule.
func TestPreApproves(t *testing.T) {
	strict := func(rs []PolicyRule) bool {
		return slices.ContainsFunc(rs, func(r PolicyRule) bool { return r.Strict })
	}
	if rs := PreApproves("Bash(git push --force origin main)", nil); !strict(rs) {
		t.Errorf("naming force push to main: %v", rs)
	}
	if rs := PreApproves("Bash(git push --force origin master)", nil); !strict(rs) {
		t.Errorf("master is always protected: %v", rs)
	}
	if rs := PreApproves("Bash(npm test)", nil); len(rs) != 0 {
		t.Errorf("npm test: %v", rs)
	}
	if rs := PreApproves("Bash(git push --force origin develop)", []string{"develop"}); !strict(rs) {
		t.Errorf("default branch: %v", rs)
	}
}

// TestDefaultBranch reads origin/HEAD, says when it cannot, and refuses a name
// that is not plain.
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
	if b, known := DefaultBranch(root); b != "" || !known {
		t.Errorf("no origin/HEAD: %q %v", b, known)
	}
	head := filepath.Join(git, "refs/remotes/origin/HEAD")
	for _, tc := range []struct {
		content, want string
		known         bool
	}{
		{"ref: refs/remotes/origin/develop\n", "develop", true},
		{"ref: refs/remotes/origin/a*b\n", "", false},
		{"ref: refs/remotes/origin/x,y\n", "", false},
		{"0123abcd\n", "", false},
	} {
		if err := os.WriteFile(head, []byte(tc.content), 0o600); err != nil {
			t.Fatal(err)
		}
		if b, known := DefaultBranch(root); b != tc.want || known != tc.known {
			t.Errorf("%q: %q %v", tc.content, b, known)
		}
	}
	if err := os.Mkdir(filepath.Join(git, "reftable"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, known := DefaultBranch(root); known {
		t.Error("a reftable repo's default branch read as known")
	}
}

// TestOpenCodePolicyRendered (T6967): on screen OpenCode's config asks before
// the policy's shell rules, after every allow so the ask wins; under --auto,
// which answers an ask, they stay denies; key file reads stay denied, and a
// template is not in the path denies.
func TestOpenCodePolicyRendered(t *testing.T) {
	has := func(rs []openCodeRule, want openCodeRule) bool { return slices.Contains(rs, want) }
	c := baseConfig()
	c.Policy = PolicyPlan{OnScreen: true}.Decide()
	d := renderDoc(t, c)
	last := d.Permissions[len(d.Permissions)-1]
	if last.Effect != "ask" || !has(d.Permissions, openCodeRule{"shell", "rm -rf*", "ask"}) {
		t.Errorf("on screen, permissions end %+v", last)
	}
	if has(d.Policies, openCodeRule{"tool.use", "shell:rm -rf*", "deny"}) || !has(d.Policies, openCodeRule{"tool.use", "read:*.pem", "deny"}) ||
		!has(d.Policies, openCodeRule{"tool.use", "shell:git push --force*main*", "deny"}) || has(d.Policies, openCodeRule{"tool.use", "read:.env.*", "deny"}) {
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
