package permissions

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

func TestRender(t *testing.T) {
	aggy := func(k Kind, text string) Rule { return Rule{Kind: k, Text: text, Origin: FromAggy} }
	s := Set{Trusted: true, Rules: []Rule{
		{Kind: Allow, Text: "Bash(git status)", Origin: FromClaudeUser},
		{Kind: Allow, Text: "Bash(git diff *)", Origin: FromClaudeLocal},
		aggy(Allow, "Bash(gh pr list)"),
		aggy(Allow, "Bash(git)"),
		aggy(Allow, "Bash(a b,c)"),
		aggy(Allow, "mcp__x__y"),
		aggy(Deny, "Bash(curl *)"),
	}}
	c := s.Render(adapter.Claude)
	if slices.Contains(c.Allow, "Bash(git status)") {
		t.Error("Claude reads its own files")
	}
	if !slices.Contains(c.Allow, "mcp__x__y") || len(c.Deny) != 1 || len(c.Skipped) != 0 {
		t.Errorf("claude: %+v", c)
	}

	g := s.Render(adapter.Grok)
	if !slices.Equal(g.Allow, []string{"Bash(gh pr list)"}) {
		t.Errorf("grok allow: %q", g.Allow)
	}
	if !slices.Equal(g.Deny, []string{"Bash(curl *)"}) || len(g.Ask) != 0 {
		t.Errorf("grok: %+v", g)
	}
	assertNamed(t, g, "Bash(git): Grok takes an exact rule as a prefix", "Bash(a b,c): Grok splits", "mcp__x__y: Grok writes")

	for _, v := range []adapter.Vendor{adapter.Codex, adapter.OpenCode} {
		if r := s.Render(v); len(r.Allow)+len(r.Ask)+len(r.Deny) != 0 {
			t.Errorf("%s: %+v", v, r)
		}
	}
}

func assertNamed(t *testing.T, r Rendered, want ...string) {
	t.Helper()
	var whys []string
	for _, k := range r.Skipped {
		whys = append(whys, k.Text+": "+k.Why)
	}
	joined := strings.Join(whys, "\n")
	for _, w := range want {
		if !strings.Contains(joined, w) {
			t.Errorf("not named: %q in\n%s", w, joined)
		}
	}
}

// TestGrokReadsRulesItsOwnWay: on Grok ? and [...] are globs, and an exact rule is
// a prefix (policy.rs:648), so each is judged as what Grok would make of it.
func TestGrokReadsRulesItsOwnWay(t *testing.T) {
	for _, tc := range []struct {
		rule    string
		trusted bool
		sent    bool
	}{
		{"Bash(?? *)", true, false},
		{"Bash(pyth?n *)", true, false},
		{"Bash(pyth[o]n *)", true, false},
		{"Bash(gh pr view ?)", true, false},
		{"Bash(sudo sh)", true, false},
		{"Bash(env python)", true, false},
		{"Bash(sudo)", true, false},
		{"Bash(git)", true, false},
		{"Bash(python3 tool.py)", true, false},
		{"Bash(npm run build)", true, false},
		{"Bash(ls)", false, true},
		{"Bash(git status)", false, true},
		{"Bash(git status)", true, true},
		// Exact, it is read-only; as a prefix, it reaches git diff --output=x.
		{"Bash(git diff --stat)", false, false},
		{"Bash(git diff --stat)", true, true},
		{"Bash(make build)", true, true},
	} {
		s := Set{Trusted: tc.trusted, Rules: []Rule{{Kind: Allow, Text: tc.rule, Origin: FromAggy}}}
		r := s.Render(adapter.Grok)
		if sent := slices.Contains(r.Allow, tc.rule); sent != tc.sent {
			t.Errorf("%s (trusted %v): sent %v: %+v", tc.rule, tc.trusted, sent, r.Skipped)
		}
		if !tc.sent && len(r.Skipped) != 1 {
			t.Errorf("%s: not named: %+v", tc.rule, r)
		}
	}
}

// TestGrokLostRuleStopsAllows: a deny or ask that cannot reach Grok might have
// stopped an allow, of any tool, so no allow from this layer goes.
func TestGrokLostRuleStopsAllows(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		rule string
	}{
		{Ask, "Bash(git push *)"},
		{Ask, "Read(./secret)"},
		{Deny, "Bash(curl a,b *)"},
		{Deny, "mcp__x__y"},
		{Deny, "Write"},
	} {
		s := Set{Trusted: true, Rules: []Rule{
			{Kind: Allow, Text: "Bash(make build)", Origin: FromAggy},
			{Kind: Allow, Text: "Read(./docs/**)", Origin: FromAggy},
			{Kind: Deny, Text: "Bash(rm x)", Origin: FromAggy},
			{Kind: tc.kind, Text: tc.rule, Origin: FromAggy},
		}}
		r := s.Render(adapter.Grok)
		if len(r.Allow) != 0 || !slices.Contains(r.Deny, "Bash(rm x)") {
			t.Errorf("%s: %+v", tc.rule, r)
		}
		assertNamed(t, r, "Bash(make build): the rule "+tc.rule+" cannot reach Grok")
		if c := s.Render(adapter.Claude); len(c.Allow) != 2 {
			t.Errorf("Claude takes every rule: %+v", c)
		}
	}
}

func TestRenderMode(t *testing.T) {
	for _, tc := range []struct {
		from Origin
		want string
	}{{FromClaudeUser, ""}, {FromClaudeLocal, ""}, {FromAggy, "auto"}, {FromConfig, "auto"}} {
		s := Set{Mode: "auto", ModeFrom: tc.from}
		if got := s.Render(adapter.Claude).Mode; got != tc.want {
			t.Errorf("%s: %q", tc.from, got)
		}
		if got := s.Render(adapter.Grok).Mode; got != tc.want {
			t.Errorf("grok %s: %q", tc.from, got)
		}
		if got := s.Render(adapter.Codex).Mode; got != "" {
			t.Errorf("codex %s: %q", tc.from, got)
		}
	}
}

func TestDashRuleNeverLoaded(t *testing.T) {
	f := newFixture(t)
	write(t, f.in.AggyFile, `{"permissions":{"deny":["--always-approve"]}}`)
	if s := Load(f.in); s.FileErr == nil || len(s.Rules) != 0 {
		t.Errorf("%+v", s)
	}
	f = newFixture(t)
	write(t, filepath.Join(f.in.ClaudeDir, "settings.json"), `{"permissions":{"deny":["--always-approve"]}}`)
	s := Load(f.in)
	if r := s.Render(adapter.Grok); len(r.Deny) != 0 || s.Withhold == "" {
		t.Errorf("%+v, %+v", r, s)
	}
}
