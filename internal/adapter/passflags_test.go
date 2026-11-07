package adapter_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
)

func TestPassFlags(t *testing.T) {
	cl, cx, gk := claude.Adapter{}.PassFlags(), codex.Adapter{}.PassFlags(), grok.New().PassFlags()
	tests := []struct {
		name     string
		p        adapter.PassFlags
		args     []string
		noPolicy bool
		want     adapter.ParsedFlags
		refused  string // error text; "" means none
	}{
		{name: "the CLI's flags pass unchanged", p: cl, args: []string{"--permission-mode", "plan", "--verbose"},
			want: adapter.ParsedFlags{Extra: []string{"--permission-mode", "plan", "--verbose"}}},
		{name: "aggy takes the model", p: cl, args: []string{"--model", "opus", "--verbose"},
			want: adapter.ParsedFlags{Model: "opus", Extra: []string{"--verbose"}}},
		{name: "--model=", p: gk, args: []string{"--model=grok-x"}, want: adapter.ParsedFlags{Model: "grok-x"}},
		{name: "codex -m", p: cx, args: []string{"-m", "gpt-x"}, want: adapter.ParsedFlags{Model: "gpt-x"}},
		{name: "codex -m stuck to its value", p: cx, args: []string{"-mgpt-x"}, want: adapter.ParsedFlags{Model: "gpt-x"}},
		{name: "a model with no name", p: cl, args: []string{"--model"}, refused: "needs a model name"},
		{name: "a model that could be a flag", p: cl, args: []string{"--model", "--session-id"}, refused: "not a model name"},
		{name: "claude -c", p: cl, args: []string{"-c"}, want: adapter.ParsedFlags{Continue: true}},
		{name: "claude --resume with an id", p: cl, args: []string{"--resume", "abcd"}, want: adapter.ParsedFlags{Resume: true, ResumeID: "abcd"}},
		{name: "claude -r with no id, then a flag", p: cl, args: []string{"-r", "--verbose"}, want: adapter.ParsedFlags{Resume: true, Extra: []string{"--verbose"}}},
		{name: "grok --resume=", p: gk, args: []string{"--resume=abcd"}, want: adapter.ParsedFlags{Resume: true, ResumeID: "abcd"}},

		{name: "claude --session-id", p: cl, args: []string{"--session-id", "x"}, refused: "--session-id clashes"},
		{name: "claude --session-id=", p: cl, args: []string{"--session-id=x"}, refused: "--session-id clashes"},
		{name: "claude --settings", p: cl, args: []string{"--settings", "{}"}, refused: "--settings clashes"},
		{name: "claude memory", p: cl, args: []string{"--append-system-prompt-file", "x"}, refused: "shared memory"},
		{name: "claude --mcp-config", p: cl, args: []string{"--mcp-config", "x"}, refused: "--mcp-config clashes"},
		{name: "claude --fork-session", p: cl, args: []string{"--fork-session"}, refused: "--fork-session clashes"},
		{name: "claude -p", p: cl, args: []string{"-p"}, refused: "-p clashes"},
		{name: "claude -p run together", p: cl, args: []string{"-dp"}, refused: "-p clashes"},
		{name: "claude -c run together", p: cl, args: []string{"-dc"}, refused: "write -c on its own"},
		{name: "claude --worktree", p: cl, args: []string{"-w", "x"}, refused: "-w clashes"},
		{name: "grok --session-id", p: gk, args: []string{"--session-id", "x"}, refused: "--session-id clashes"},
		{name: "grok --deny", p: gk, args: []string{"--deny", "x"}, refused: "--deny clashes"},
		{name: "grok --rules", p: gk, args: []string{"--rules", "x"}, refused: "--rules clashes"},
		{name: "grok --agent", p: gk, args: []string{"--agent", "x"}, refused: "--agent clashes"},
		{name: "grok --no-alt-screen", p: gk, args: []string{"--no-alt-screen"}, refused: "--no-alt-screen clashes"},
		{name: "codex --no-alt-screen", p: cx, args: []string{"--no-alt-screen"}, refused: "--no-alt-screen clashes"},
		{name: "codex -C is not -c", p: cx, args: []string{"-C", "/x"}, refused: "-C clashes"},

		// -c is Codex's config override: aggy's continue is --continue there.
		{name: "codex -c passes", p: cx, args: []string{"-c", "foo=bar", "--search"}, want: adapter.ParsedFlags{Extra: []string{"-c", "foo=bar", "--search"}}},
		{name: "codex --config= passes", p: cx, args: []string{"--config=foo=bar"}, want: adapter.ParsedFlags{Extra: []string{"--config=foo=bar"}}},
		{name: "codex -c memory", p: cx, args: []string{"-c", "developer_instructions=x"}, refused: "shared memory"},
		{name: "codex -c stuck to its value", p: cx, args: []string{"-cdeveloper_instructions=x"}, refused: "shared memory"},
		{name: "codex --config= memory", p: cx, args: []string{"--config=developer_instructions=x"}, refused: "shared memory"},
		{name: "codex -c quoted key", p: cx, args: []string{"-c", `"model"="x"`}, refused: "use --model"},
		{name: "codex -c a server", p: cx, args: []string{"-c", "mcp_servers.x.command=y"}, refused: "managed tools"},
		{name: "codex -c with no value", p: cx, args: []string{"-c"}, refused: "needs a value"},
		{name: "codex --continue", p: cx, args: []string{"--continue"}, want: adapter.ParsedFlags{Continue: true}},

		// The bypass flag may lose the rules file, so it asks for --no-policy.
		{name: "codex --yolo", p: cx, args: []string{"--yolo"}, refused: "--no-policy"},
		{name: "codex --yolo with --no-policy", p: cx, args: []string{"--yolo"}, noPolicy: true, want: adapter.ParsedFlags{Extra: []string{"--yolo"}}},
		// Deny rules hold in every Claude mode, so this one passes.
		{name: "claude bypass passes", p: cl, args: []string{"--permission-mode", "bypassPermissions"},
			want: adapter.ParsedFlags{Extra: []string{"--permission-mode", "bypassPermissions"}}},

		{name: "a prompt", p: cl, args: []string{"fix the bug"}, refused: "not a flag"},
		{name: "a subcommand", p: cx, args: []string{"resume", "abc"}, refused: "not a flag"},
		{name: "a word after a flag's value", p: cl, args: []string{"--permission-mode", "plan", "fix"}, refused: "not a flag"},
		{name: "a word after --flag=value", p: cl, args: []string{"--permission-mode=plan", "fix"}, refused: "not a flag"},
		{name: "a word after a model", p: cl, args: []string{"--model", "opus", "fix"}, refused: "not a flag"},
		{name: "--", p: cl, args: []string{"--", "fix"}, refused: "starts a prompt"},
		{name: "-", p: cl, args: []string{"-"}, refused: "not a flag"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.p.Parse(tt.args, tt.noPolicy)
			if tt.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tt.refused) {
					t.Fatalf("err = %v, want %q", err, tt.refused)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Model != tt.want.Model || got.Continue != tt.want.Continue || got.Resume != tt.want.Resume ||
				got.ResumeID != tt.want.ResumeID || !slices.Equal(got.Extra, tt.want.Extra) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
