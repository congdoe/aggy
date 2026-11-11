package parity

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
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

func kindOf(ds []PolicyDecision, text string) permissions.Kind {
	for _, d := range ds {
		if d.Text == text {
			return d.Kind
		}
	}
	return ""
}

// TestPolicyPlanBorrow: a headless plan (a borrow child, a headless run,
// print mode) gets exactly the deny list it had before T6967, whatever
// settings say, a group turned off (allow, T6979) included.
func TestPolicyPlanBorrow(t *testing.T) {
	allOff := map[string]permissions.Kind{}
	for _, g := range permissions.PolicyGroups {
		allOff[g] = permissions.Allow
	}
	tests := []struct {
		name     string
		settings map[string]permissions.Kind
	}{
		{"nothing set", nil},
		{"an ask", map[string]permissions.Kind{permissions.PolicyRmRf: permissions.Ask}},
		{"one group off", map[string]permissions.Kind{permissions.PolicyKeys: permissions.Allow}},
		{"every group off", allOff},
	}
	for _, tt := range tests {
		plan := PolicyPlan{Branches: []string{"develop"}, Settings: tt.settings}
		ask, deny := ClaudePolicy(plan.Decide())
		if len(ask) != 0 || !slices.Equal(deny, ClaudeDeny) {
			t.Errorf("%s: claude borrow: ask %v, deny %v", tt.name, ask, deny)
		}
		if got := GrokPolicy(plan.Decide()); !slices.Equal(got, GrokDeny) {
			t.Errorf("%s: grok borrow: %v", tt.name, got)
		}
		oAsk, oDeny := OpenCodePolicy(plan.Decide(), false)
		if len(oAsk) != 0 || !slices.Contains(oDeny, "shell:rm -rf*") || !slices.Contains(oDeny, "shell:git push --force*") {
			t.Errorf("%s: opencode borrow: ask %v deny %v", tt.name, oAsk, oDeny)
		}
		if !plan.StrictMatch(Call{Kind: CallShell, Text: "git push --force origin main"}) {
			t.Errorf("%s: a headless plan lost a strict match", tt.name)
		}
	}
}

// TestPolicyGroupOff: a group set to allow on screen (T6979) has no rule on
// any CLI and no opinion from the decision hook; the other groups keep theirs.
func TestPolicyGroupOff(t *testing.T) {
	tests := []struct {
		name       string
		off        string
		gone, kept []string // Claude spellings
		call       string
	}{
		{"rm-rf", permissions.PolicyRmRf, []string{"Bash(rm -rf*)"}, []string{"Bash(git reset --hard*)", "Read(//**/.env)"}, "rm -rf build"},
		{"key-read, strict", permissions.PolicyKeys, []string{"Read(//**/*.pem)"}, []string{"Bash(git push --force*main*)"}, "cat server.pem"},
		{"force-push-main, strict", permissions.PolicyForceMain, []string{"Bash(git push --force*main*)"}, []string{"Bash(git push --force*)"}, ""},
	}
	for _, tt := range tests {
		plan := PolicyPlan{OnScreen: true, Settings: map[string]permissions.Kind{tt.off: permissions.Allow}}
		ds := plan.Decide()
		ask, deny := ClaudePolicy(ds)
		grok := GrokPolicy(ds)
		oAsk, oDeny := OpenCodePolicyFor(ds, false, true)
		for _, r := range tt.gone {
			if slices.Contains(ask, r) || slices.Contains(deny, r) || slices.Contains(grok, grokSpelling(r)) {
				t.Errorf("%s: %s still rendered: ask %v deny %v grok %v", tt.name, r, ask, deny, grok)
			}
			if cmd, ok := strings.CutPrefix(r, "Bash("); ok {
				cmd = strings.TrimSuffix(cmd, ")")
				if slices.Contains(oAsk, cmd) || slices.Contains(oDeny, "shell:"+cmd) {
					t.Errorf("%s: %s still on OpenCode: ask %v deny %v", tt.name, r, oAsk, oDeny)
				}
			}
		}
		for _, r := range tt.kept {
			if !slices.Contains(ask, r) || !slices.Contains(grok, grokSpelling(r)) {
				t.Errorf("%s: %s lost: ask %v grok %v", tt.name, r, ask, grok)
			}
		}
		if tt.call != "" {
			if d := plan.DecideCall(Call{Kind: CallShell, Text: tt.call}, PreApproved{}); d.Verdict != VerdictNone {
				t.Errorf("%s: the hook has an opinion on %q: %+v", tt.name, tt.call, d)
			}
		}
		if tt.off == permissions.PolicyForceMain {
			c := Call{Kind: CallShell, Text: "git push --force origin main"}
			if plan.StrictMatch(c) {
				t.Errorf("%s: still strict", tt.name)
			}
			if d := plan.DecideCall(c, PreApproved{}); d.Verdict != VerdictAsk || d.Strict {
				t.Errorf("%s: the other push groups should still ask, not strict: %+v", tt.name, d)
			}
		}
	}
}

// TestPolicyPlanOnScreen: every rule asks, the protected branches and the
// default branch are strict, and a group set to deny is a deny.
func TestPolicyPlanOnScreen(t *testing.T) {
	const (
		rmRf      = "Bash(rm -rf*)"
		pushMain  = "Bash(git push * main)"
		force     = "Bash(git push --force*)"
		forceMain = "Bash(git push --force*main*)"
		forceDev  = "Bash(git push --force*develop*)"
		forceMast = "Bash(git push --force*master*)"
		forceTrnk = "Bash(git push --force*trunk*)"
		pem       = "Read(//**/*.pem)"
		env       = "Read(//**/.env)"
		ask, deny = permissions.Ask, permissions.Deny
	)
	tests := []struct {
		name     string
		settings map[string]permissions.Kind
		want     map[string]permissions.Kind
	}{
		{"nothing set", nil, map[string]permissions.Kind{rmRf: ask, pushMain: ask, force: ask, forceMain: ask, forceDev: ask, forceMast: ask, forceTrnk: ask, pem: ask, env: ask}},
		{"a deny setting", map[string]permissions.Kind{permissions.PolicyRmRf: deny}, map[string]permissions.Kind{rmRf: deny, pushMain: ask}},
		{"force push denied keeps the strict rules asking", map[string]permissions.Kind{permissions.PolicyForce: deny}, map[string]permissions.Kind{force: deny, forceMain: ask}},
	}
	for _, tt := range tests {
		ds := PolicyPlan{OnScreen: true, Settings: tt.settings, Branches: []string{"develop"}}.Decide()
		for text, want := range tt.want {
			if got := kindOf(ds, text); got != want {
				t.Errorf("%s: %s = %q, want %q", tt.name, text, got, want)
			}
		}
		for _, d := range ds {
			if d.Kind != ask && d.Kind != deny {
				t.Errorf("%s: %s = %q, neither ask nor deny", tt.name, d.Text, d.Kind)
			}
		}
	}
}

// TestPolicyRenderings (review 2, 6): Grok gets every rule as a deny,
// OpenCode asks (a deny under --auto), and OpenCode's strict rules are denies.
func TestPolicyRenderings(t *testing.T) {
	ds := PolicyPlan{OnScreen: true}.Decide()
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
	oAsk, oDeny = OpenCodePolicy(ds, true)
	if len(oAsk) != 0 || !slices.Contains(oDeny, "shell:rm -rf*") || !slices.Contains(oDeny, "shell:git reset --hard*") {
		t.Errorf("opencode --auto: ask %v deny %v", oAsk, oDeny)
	}
}

// TestCoveredRules: which rules an allow names or covers, for add's line,
// Grok's allow filter, and suggest. A force push to another branch is an
// ordinary rule, not a strict one.
func TestCoveredRules(t *testing.T) {
	strict := func(rs []PolicyRule) bool {
		return slices.ContainsFunc(rs, func(r PolicyRule) bool { return r.Strict })
	}
	for _, tc := range []struct {
		allow    string
		branches []string
		any      bool
		strict   bool
	}{
		{"Bash(git push --force origin main)", nil, true, true},
		{"Bash(git push --force origin master)", nil, true, true},
		{"Bash(git push --force origin trunk)", nil, true, true},
		{"Bash(git push --force origin develop)", []string{"develop"}, true, true},
		{"Bash(git push --force origin feature)", nil, true, false},
		{"Bash(git push *)", nil, true, true},
		{"Bash(rm -rf dist)", nil, true, false},
		{"Bash(rm:*)", nil, true, false},
		{"Read(./certs/server.pem)", nil, true, true},
		{"Read(./.env.example)", nil, false, false},
		{"Bash(npm test)", nil, false, false},
	} {
		rs := CoveredRules(tc.allow, tc.branches)
		if (len(rs) > 0) != tc.any || strict(rs) != tc.strict {
			t.Errorf("%s: %v", tc.allow, rs)
		}
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
