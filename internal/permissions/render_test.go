package permissions

import (
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

func TestRender(t *testing.T) {
	aggy := func(k Kind, text string) Rule { return Rule{Kind: k, Text: text, Origin: FromAggy} }
	s := Set{Rules: []Rule{
		{Kind: Allow, Text: "Bash(git status)", Origin: FromClaudeUser},
		{Kind: Allow, Text: "Bash(git diff *)", Origin: FromClaudeLocal},
		aggy(Allow, "Bash(gh pr list)"),
		aggy(Allow, "Bash(git)"),
		aggy(Allow, "Bash(a b,c)"),
		aggy(Allow, "mcp__x__y"),
		aggy(Allow, "Bash(git push origin x)"),
		aggy(Ask, "Bash(git push *)"),
		aggy(Deny, "Bash(curl *)"),
		aggy(Deny, "Bash(x y,z)"),
	}}
	c := s.Render(adapter.Claude)
	if slices.Contains(c.Allow, "Bash(git status)") {
		t.Error("Claude reads its own files")
	}
	if !slices.Contains(c.Allow, "mcp__x__y") || !slices.Equal(c.Ask, []string{"Bash(git push *)"}) || len(c.Deny) != 2 || len(c.Skipped) != 0 {
		t.Errorf("claude: %+v", c)
	}

	g := s.Render(adapter.Grok)
	if !slices.Equal(g.Allow, []string{"Bash(gh pr list)"}) {
		t.Errorf("grok allow: %q", g.Allow)
	}
	if !slices.Equal(g.Deny, []string{"Bash(curl *)"}) || len(g.Ask) != 0 {
		t.Errorf("grok: %+v", g)
	}
	var whys []string
	for _, k := range g.Skipped {
		whys = append(whys, k.Text+": "+k.Why)
	}
	joined := strings.Join(whys, "\n")
	for _, w := range []string{"Bash(git push *): Grok has no flag", "Bash(git): Grok would take one word", "Bash(a b,c): Grok splits", "mcp__x__y: Grok writes", "Bash(git push origin x): it overlaps", "Bash(x y,z): Grok splits"} {
		if !strings.Contains(joined, w) {
			t.Errorf("not named: %q in\n%s", w, joined)
		}
	}

	for _, v := range []adapter.Vendor{adapter.Codex, adapter.OpenCode} {
		if r := s.Render(v); len(r.Allow)+len(r.Ask)+len(r.Deny) != 0 {
			t.Errorf("%s: %+v", v, r)
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

func TestOverlap(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"Bash(git push x)", "Bash(git push *)", true},
		{"Bash(git *)", "Bash(git push *)", true},
		{"Bash(git *)", "Bash(git push)", true},
		{"Bash(git status)", "Bash(git push *)", false},
		{"Bash(git status)", "Bash(git status)", true},
		{"Bash(git status)", "Bash(git status -s)", false},
		{"Read", "Bash(git *)", false},
		{"Read", "Read", true},
	} {
		if got := Overlap(tc.a, tc.b); got != tc.want {
			t.Errorf("%s, %s: %v", tc.a, tc.b, got)
		}
	}
}
