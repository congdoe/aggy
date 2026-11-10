package grok

import (
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestInteractivePersonsFlags (T6962): the person's --rules join the shared
// memory in aggy's one --rules, their --deny adds to aggy's, and their --agent
// passes only when aggy passes no agent of its own.
func TestInteractivePersonsFlags(t *testing.T) {
	const id = "a0000000-0000-4000-8000-000000000000"
	tests := []struct {
		name    string
		req     adapter.InteractiveRequest
		check   func(args []string) bool
		refused string
	}{
		{
			name: "rules join the memory",
			req:  adapter.InteractiveRequest{Memory: "MEM", Extra: []string{"--rules", "be terse", "--append-system-prompt=cite"}},
			check: func(a []string) bool {
				i := slices.Index(a, "--rules")
				return i >= 0 && a[i+1] == "be terse\n\ncite\n\nMEM" && strings.Count(strings.Join(a, " "), "--rules") == 1 &&
					!slices.Contains(a, "--append-system-prompt=cite")
			},
		},
		{
			name:  "rules with no memory pass as given",
			req:   adapter.InteractiveRequest{Extra: []string{"--rules", "be terse"}},
			check: func(a []string) bool { return slices.Equal(a[len(a)-2:], []string{"--rules", "be terse"}) },
		},
		{
			name: "deny adds to aggy's",
			req:  adapter.InteractiveRequest{Deny: []string{"Bash(rm -rf*)"}, Extra: []string{"--deny", "Read(x)"}},
			check: func(a []string) bool {
				return strings.Contains(strings.Join(a, " "), "--deny Bash(rm -rf*)") && slices.Equal(a[len(a)-2:], []string{"--deny", "Read(x)"})
			},
		},
		{
			name:  "an agent with no tools on",
			req:   adapter.InteractiveRequest{Extra: []string{"--agent", "mine"}},
			check: func(a []string) bool { return slices.Equal(a[len(a)-2:], []string{"--agent", "mine"}) },
		},
		{
			name:    "an agent with aggy's",
			req:     adapter.InteractiveRequest{AgentFile: "/run/agent.md", Extra: []string{"--agent=mine"}},
			refused: "aggy tool off",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.Cwd, tt.req.SessionID = "/w", id
			c, err := New().Interactive(tt.req)
			if tt.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tt.refused) {
					t.Fatalf("err = %v, want %q", err, tt.refused)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !tt.check(c.Args) {
				t.Errorf("args %q", c.Args)
			}
		})
	}
}

// TestPrintDropsAltScreen (T6961): a print run is not on screen, so it gets no
// --no-alt-screen.
func TestPrintDropsAltScreen(t *testing.T) {
	c, err := New().Interactive(adapter.InteractiveRequest{Cwd: "/w", Print: true, Extra: []string{"-p", "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(c.Args, "--no-alt-screen") || !slices.Equal(c.Args, []string{"-p", "hi"}) {
		t.Errorf("args %q", c.Args)
	}
}
