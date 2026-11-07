package adapter_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
)

// TestPassFlagsLoosening: whatever loosens approvals, the sandbox, or hook
// trust is refused without --no-policy, however it is spelled, and with it
// goes through, named, and left out of Safe.
func TestPassFlagsLoosening(t *testing.T) {
	cl, cx, gk := claude.Adapter{}.PassFlags(), codex.Adapter{}.PassFlags(), grok.New().PassFlags()
	tests := []struct {
		name string
		p    adapter.PassFlags
		args []string
	}{
		{"claude skip permissions", cl, []string{"--dangerously-skip-permissions"}},
		{"claude allow skip permissions", cl, []string{"--allow-dangerously-skip-permissions"}},
		{"claude a bypass mode", cl, []string{"--permission-mode", "bypassPermissions"}},
		{"claude a bypass mode, =", cl, []string{"--permission-mode=bypassPermissions"}},
		{"claude an unknown mode", cl, []string{"--permission-mode", "newMode"}},
		{"codex hook trust", cx, []string{"--dangerously-bypass-hook-trust"}},
		{"codex bypass", cx, []string{"--dangerously-bypass-approvals-and-sandbox"}},
		{"codex --yolo", cx, []string{"--yolo"}},
		{"codex --approve-for-me", cx, []string{"--approve-for-me"}},
		{"codex -a never", cx, []string{"-a", "never"}},
		{"codex -anever", cx, []string{"-anever"}},
		{"codex --ask-for-approval=never", cx, []string{"--ask-for-approval=never"}},
		{"codex -s full", cx, []string{"-s", "danger-full-access"}},
		{"codex --sandbox=full", cx, []string{"--sandbox=danger-full-access"}},
		{"codex -c approval_policy", cx, []string{"-c", `approval_policy="never"`}},
		{"codex -c sandbox_mode", cx, []string{"-c", `sandbox_mode="danger-full-access"`}},
		{"codex -c quoted", cx, []string{"--config", `"sandbox_mode"="danger-full-access"`}},
		{"codex -c a hook", cx, []string{"-chooks.x=y"}},
		{"codex -c anything else", cx, []string{"-c", "foo=bar"}},
		{"grok dangerously", gk, []string{"--dangerously-anything"}},
		{"grok --yolo (unverified)", gk, []string{"--yolo"}},
		{"grok a bypass mode (unverified)", gk, []string{"--permission-mode", "bypassPermissions"}},
		{"grok a sandbox off (unverified)", gk, []string{"--sandbox", "off"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := tt.p.Parse(tt.args, false); !errors.Is(err, adapter.ErrLoosens) {
				t.Fatalf("without --no-policy: %+v, %v; want ErrLoosens", got, err)
			}
			args := append([]string{"--verbose"}, tt.args...)
			got, err := tt.p.Parse(args, true)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.Extra, args) || !slices.Equal(got.Safe, []string{"--verbose"}) || len(got.Loosens) != 1 {
				t.Errorf("with --no-policy: %+v", got)
			}
		})
	}
}

func TestPassFlags(t *testing.T) {
	cl, cx, gk := claude.Adapter{}.PassFlags(), codex.Adapter{}.PassFlags(), grok.New().PassFlags()
	tests := []struct {
		name     string
		p        adapter.PassFlags
		args     []string
		noPolicy bool
		want     adapter.ParsedFlags
		refused  string // error text; "" means none
		loosens  bool   // refused, for --no-policy to let through
	}{
		{name: "the CLI's flags pass unchanged", p: cl, args: []string{"--permission-mode", "plan", "--verbose"},
			want: adapter.ParsedFlags{Extra: []string{"--permission-mode", "plan", "--verbose"}, Safe: []string{"--permission-mode", "plan", "--verbose"}}},
		{name: "aggy takes the model", p: cl, args: []string{"--model", "opus", "--verbose"},
			want: adapter.ParsedFlags{Model: "opus", Extra: []string{"--verbose"}, Safe: []string{"--verbose"}}},
		{name: "--model=", p: gk, args: []string{"--model=grok-x"}, want: adapter.ParsedFlags{Model: "grok-x"}},
		{name: "codex -m", p: cx, args: []string{"-m", "gpt-x"}, want: adapter.ParsedFlags{Model: "gpt-x"}},
		{name: "codex -m stuck to its value", p: cx, args: []string{"-mgpt-x"}, want: adapter.ParsedFlags{Model: "gpt-x"}},
		{name: "a model with no name", p: cl, args: []string{"--model"}, refused: "needs a model name"},
		{name: "a model that could be a flag", p: cl, args: []string{"--model", "--session-id"}, refused: "not a model name"},
		{name: "claude -c", p: cl, args: []string{"-c"}, want: adapter.ParsedFlags{Continue: true}},
		{name: "claude --resume with an id", p: cl, args: []string{"--resume", "abcd"}, want: adapter.ParsedFlags{Resume: true, ResumeID: "abcd"}},
		{name: "claude -r with no id, then a flag", p: cl, args: []string{"-r", "--verbose"}, want: adapter.ParsedFlags{Resume: true, Extra: []string{"--verbose"}, Safe: []string{"--verbose"}}},
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
		{name: "codex -c of a harmless key passes", p: cx, args: []string{"-c", "model_verbosity=low", "--search"},
			want: adapter.ParsedFlags{Extra: []string{"-c", "model_verbosity=low", "--search"}, Safe: []string{"-c", "model_verbosity=low", "--search"}}},
		{name: "codex --config= of a harmless key passes", p: cx, args: []string{"--config=tui.theme=x"},
			want: adapter.ParsedFlags{Extra: []string{"--config=tui.theme=x"}, Safe: []string{"--config=tui.theme=x"}}},
		{name: "codex -c memory", p: cx, args: []string{"-c", "developer_instructions=x"}, refused: "shared memory"},
		{name: "codex -c stuck to its value", p: cx, args: []string{"-cdeveloper_instructions=x"}, refused: "shared memory"},
		{name: "codex --config= memory", p: cx, args: []string{"--config=developer_instructions=x"}, refused: "shared memory"},
		{name: "codex -c quoted key", p: cx, args: []string{"-c", `"model"="x"`}, refused: "use --model"},
		{name: "codex -c a server", p: cx, args: []string{"-c", "mcp_servers.x.command=y"}, refused: "managed tools"},
		{name: "codex -c with no value", p: cx, args: []string{"-c"}, refused: "needs a value"},
		{name: "codex --continue", p: cx, args: []string{"--continue"}, want: adapter.ParsedFlags{Continue: true}},

		{name: "a -c needs a value to classify", p: cx, args: []string{"-c", "model_verbosity"}, loosens: true},
		{name: "a value flag takes a word", p: cl, args: []string{"--add-dir", "/x"},
			want: adapter.ParsedFlags{Extra: []string{"--add-dir", "/x"}, Safe: []string{"--add-dir", "/x"}}},
		{name: "--flag=value needs no table entry", p: cl, args: []string{"--newflag=x"},
			want: adapter.ParsedFlags{Extra: []string{"--newflag=x"}, Safe: []string{"--newflag=x"}}},

		// Only a flag known to take a value takes the next word.
		{name: "a prompt after a switch", p: cl, args: []string{"--verbose", "fix the bug"}, refused: "not a flag"},
		{name: "a subcommand after a switch", p: cx, args: []string{"--search", "resume", "--last"}, refused: "not a flag"},
		{name: "a word after an unknown flag", p: gk, args: []string{"--newflag", "x"}, refused: "not a flag"},
		{name: "two words after a value flag", p: cl, args: []string{"--add-dir", "/x", "fix"}, refused: "not a flag"},

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
			if tt.loosens {
				if !errors.Is(err, adapter.ErrLoosens) {
					t.Fatalf("err = %v, want ErrLoosens", err)
				}
				return
			}
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
				got.ResumeID != tt.want.ResumeID || !slices.Equal(got.Extra, tt.want.Extra) || !slices.Equal(got.Safe, tt.want.Safe) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}
