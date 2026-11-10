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
		{"codex --disable hooks", cx, []string{"--disable", "hooks"}},
		{"codex --disable=hooks", cx, []string{"--disable=hooks"}},
		{"codex --disable the old hooks name", cx, []string{"--disable", "codex_hooks"}},
		{"codex --disable a sandbox backend", cx, []string{"--disable", "use_linux_sandbox_bwrap"}},
		{"codex --disable credential masking", cx, []string{"--disable", "credential_masking"}},
		{"codex --disable secret auth storage", cx, []string{"--disable", "secret_auth_storage"}},
		{"codex --disable code mode only", cx, []string{"--disable", "code_mode_only"}},
		{"codex -c features.hooks=false", cx, []string{"-c", "features.hooks=false"}},
		{"codex -c features.hooks with spaces", cx, []string{"-c", "features.hooks = false"}},
		{"codex -c quoted features.hooks", cx, []string{"-c", `features."hooks"=false`}},
		{"codex -c features as a table", cx, []string{"-c", "features={hooks=false}"}},
		{"codex -c features on", cx, []string{"-c", "features.anything=true"}},
		{"codex -a on-failure", cx, []string{"-a", "on-failure"}},
		{"claude --agents with a bypass", cl, []string{"--agents", `{"a":{"permissionMode":"bypassPermissions"}}`}},
		{"claude --agents with an escaped bypass", cl, []string{"--agents", `{"a":{"permissionMode":"byp\u0061ssPermissions"}}`}},
		{"claude --agents with an escaped key", cl, []string{"--agents", `{"a":{"permission\u004dode":"newMode"}}`}},
		{"claude --agents with a repeated key", cl, []string{"--agents", `{"a":{"permissionMode":"plan","permissionMode":"bypassPermissions"}}`}},
		{"claude --agents with a mode that is no string", cl, []string{"--agents", `{"a":{"permissionMode":{"x":"y"}}}`}},
		{"claude --agents not JSON", cl, []string{"--agents", "agents.json"}},
		{"claude --agents cut short", cl, []string{"--agents", `{"a":`}},
		{"grok dangerously", gk, []string{"--dangerously-anything"}},
		{"grok --always-approve", gk, []string{"--always-approve"}},
		{"grok --auto-approve, not in its help", gk, []string{"--auto-approve"}},
		{"grok a later bypass name", gk, []string{"--bypass-all"}},
		{"grok --agents with a bypass", gk, []string{"--agents", `{"a":{"permissionMode":"bypassPermissions"}}`}},
		{"grok --agents with an escaped bypass", gk, []string{"--agents", `{"a":{"permissionMode":"byp\u0061ssPermissions"}}`}},
		{"grok --agents auto", gk, []string{"--agents", `{"a":{"permissionMode":"auto"}}`}},
		{"grok --agents dontAsk", gk, []string{"--agents", `{"a":{"permission_mode":"dontAsk"}}`}},
		{"grok --agents not JSON", gk, []string{"--agents", "agents.json"}},
		{"grok --trust, hidden", gk, []string{"--trust"}},
		{"grok --trust-folder, hidden", gk, []string{"--trust-folder"}},
		{"grok its bypass alias", gk, []string{"--dangerously-skip-permissions"}},
		{"grok a later approval name", gk, []string{"--no-approval"}},
		{"grok a later sandbox name", gk, []string{"--no-sandbox"}},
		{"grok a later trust name", gk, []string{"--trust-all"}},
		{"grok a later unsafe name", gk, []string{"--unsafe-tools"}},
		{"grok a later insecure name", gk, []string{"--insecure"}},
		{"claude --agents with a mode after a word that looks like its key", cl, []string{"--agents", `{"a":{"prompt":"permissionMode","permissionMode":"newMode"}}`}},
		{"claude --agents with a mode in an array", cl, []string{"--agents", `{"a":{"permissionMode":["plan"]}}`}},
		{"grok a later permission name", gk, []string{"--skip-permission-checks"}},
		{"grok --permission-mode auto", gk, []string{"--permission-mode", "auto"}},
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
		// T6962: merged with aggy's own at launch, so they pass here.
		{name: "claude --settings", p: cl, args: []string{"--settings", "{}"},
			want: adapter.ParsedFlags{Extra: []string{"--settings", "{}"}, Safe: []string{"--settings", "{}"}}},
		{name: "claude memory", p: cl, args: []string{"--append-system-prompt-file", "x"},
			want: adapter.ParsedFlags{Extra: []string{"--append-system-prompt-file", "x"}, Safe: []string{"--append-system-prompt-file", "x"}}},
		{name: "claude --mcp-config, as many as given", p: cl, args: []string{"--mcp-config", "x", "y"},
			want: adapter.ParsedFlags{Extra: []string{"--mcp-config", "x", "y"}, Safe: []string{"--mcp-config", "x", "y"}}},
		{name: "claude --fork-session", p: cl, args: []string{"--fork-session"}, refused: "--fork-session clashes"},
		// T6961: print mode is the caller's choice (PrintRequested); Parse marks it.
		{name: "claude -p", p: cl, args: []string{"-p"},
			want: adapter.ParsedFlags{Print: true, Extra: []string{"-p"}, Safe: []string{"-p"}}},
		// PR #124 review: a cluster is not split to find print mode.
		{name: "claude -p run together", p: cl, args: []string{"-dp"},
			want: adapter.ParsedFlags{Extra: []string{"-dp"}, Safe: []string{"-dp"}}},
		{name: "claude -c run together", p: cl, args: []string{"-dc"}, refused: "write -c on its own"},
		{name: "claude --worktree, for the first launch only", p: cl, args: []string{"-w", "x"},
			want: adapter.ParsedFlags{Extra: []string{"-w", "x"}}},
		{name: "grok --session-id", p: gk, args: []string{"--session-id", "x"}, refused: "--session-id clashes"},
		{name: "grok --deny adds to aggy's", p: gk, args: []string{"--deny", "x"},
			want: adapter.ParsedFlags{Extra: []string{"--deny", "x"}, Safe: []string{"--deny", "x"}}},
		{name: "grok --rules joins the memory", p: gk, args: []string{"--rules", "x"},
			want: adapter.ParsedFlags{Extra: []string{"--rules", "x"}, Safe: []string{"--rules", "x"}}},
		// PR #124 review: refused again until its file is read (TODO(T6895)).
		{name: "grok --agent", p: gk, args: []string{"--agent", "x"}, refused: "--agent clashes"},
		{name: "claude --desktop", p: cl, args: []string{"--desktop"}, refused: "outside this terminal"},
		{name: "claude --environment", p: cl, args: []string{"--environment", "e1"}, refused: "outside this terminal"},
		{name: "claude --bg", p: cl, args: []string{"--bg", "x"}, refused: "background worker"},
		{name: "claude --tmux", p: cl, args: []string{"-w", "x", "--tmux"}, refused: "background worker"},
		{name: "codex --remote", p: cx, args: []string{"--remote", "ws://x"}, refused: "remote app server"},
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
		{name: "codex --disable of a tool passes", p: cx, args: []string{"--disable", "web_search"},
			want: adapter.ParsedFlags{Extra: []string{"--disable", "web_search"}, Safe: []string{"--disable", "web_search"}}},
		{name: "codex -c features off, of a tool, passes", p: cx, args: []string{"-c", "features.web_search=false"},
			want: adapter.ParsedFlags{Extra: []string{"-c", "features.web_search=false"}, Safe: []string{"-c", "features.web_search=false"}}},
		{name: "codex -a untrusted tightens", p: cx, args: []string{"-a", "untrusted"},
			want: adapter.ParsedFlags{Extra: []string{"-a", "untrusted"}, Safe: []string{"-a", "untrusted"}}},
		{name: "codex --add-dir of a project", p: cx, args: []string{"--add-dir", "/srv/other"},
			want: adapter.ParsedFlags{Extra: []string{"--add-dir", "/srv/other"}, Safe: []string{"--add-dir", "/srv/other"}}},
		{name: "claude manual tightens", p: cl, args: []string{"--permission-mode", "manual"},
			want: adapter.ParsedFlags{Extra: []string{"--permission-mode", "manual"}, Safe: []string{"--permission-mode", "manual"}}},
		{name: "claude --allowedTools passes", p: cl, args: []string{"--allowedTools", "Bash"},
			want: adapter.ParsedFlags{Extra: []string{"--allowedTools", "Bash"}, Safe: []string{"--allowedTools", "Bash"}}},
		{name: "claude --permission-mode auto passes", p: cl, args: []string{"--permission-mode", "auto"},
			want: adapter.ParsedFlags{Extra: []string{"--permission-mode", "auto"}, Safe: []string{"--permission-mode", "auto"}}},
		{name: "claude --agents takes its word", p: cl, args: []string{"--agents", "{}"},
			want: adapter.ParsedFlags{Extra: []string{"--agents", "{}"}, Safe: []string{"--agents", "{}"}}},
		{name: "codex --profile passes", p: cx, args: []string{"--profile", "x"},
			want: adapter.ParsedFlags{Extra: []string{"--profile", "x"}, Safe: []string{"--profile", "x"}}},
		{name: "codex -p stuck to its value passes", p: cx, args: []string{"-px"},
			want: adapter.ParsedFlags{Extra: []string{"-px"}, Safe: []string{"-px"}}},
		{name: "codex --enable passes", p: cx, args: []string{"--enable", "x"},
			want: adapter.ParsedFlags{Extra: []string{"--enable", "x"}, Safe: []string{"--enable", "x"}}},
		{name: "codex --profile takes its word only", p: cx, args: []string{"--profile", "x", "fix"},
			want: adapter.ParsedFlags{Extra: []string{"--profile", "x", "fix"}, Safe: []string{"--profile", "x"}, Prompt: []string{"fix"}}},
		{name: "claude --agents with a mode that asks", p: cl, args: []string{"--agents", `{"a":{"permissionMode":"auto","prompt":"say auto"}}`},
			want: adapter.ParsedFlags{Extra: []string{"--agents", `{"a":{"permissionMode":"auto","prompt":"say auto"}}`}, Safe: []string{"--agents", `{"a":{"permissionMode":"auto","prompt":"say auto"}}`}}},
		{name: "grok --agents with plan", p: gk, args: []string{"--agents", `{"a":{"permissionMode":"plan","model":"auto"}}`},
			want: adapter.ParsedFlags{Extra: []string{"--agents", `{"a":{"permissionMode":"plan","model":"auto"}}`}, Safe: []string{"--agents", `{"a":{"permissionMode":"plan","model":"auto"}}`}}},
		{name: "codex --enable takes no flag for its value", p: cx, args: []string{"--enable", "--yolo"}, refused: "looks like a flag"},
		{name: "codex --enable= takes no flag", p: cx, args: []string{"--enable=--yolo"}, refused: "looks like a flag"},
		{name: "codex -p takes no flag", p: cx, args: []string{"-p", "--dangerously-bypass-approvals-and-sandbox"}, refused: "looks like a flag"},
		{name: "codex -p takes no flag, even with --no-policy", p: cx, args: []string{"-p", "--yolo"}, noPolicy: true, refused: "looks like a flag"},
		{name: "codex --add-dir takes no flag", p: cx, args: []string{"--add-dir", "--yolo"}, refused: "looks like a flag"},
		{name: "codex --disable takes no flag", p: cx, args: []string{"--disable", "--yolo"}, refused: "looks like a flag"},
		{name: "grok --add-dir takes no flag", p: gk, args: []string{"--add-dir", "--always-approve"}, refused: "looks like a flag"},
		{name: "claude --agents, a value that says permissionMode is no key", p: cl, args: []string{"--agents", `{"a":{"prompt":"permissionMode","model":"sonnet"}}`},
			want: adapter.ParsedFlags{Extra: []string{"--agents", `{"a":{"prompt":"permissionMode","model":"sonnet"}}`}, Safe: []string{"--agents", `{"a":{"prompt":"permissionMode","model":"sonnet"}}`}}},
		{name: "claude --agents, an array and a nested object", p: cl, args: []string{"--agents", `{"a":{"tools":["permissionMode","x"],"b":{},"model":"sonnet","permissionMode":"plan"}}`},
			want: adapter.ParsedFlags{Extra: []string{"--agents", `{"a":{"tools":["permissionMode","x"],"b":{},"model":"sonnet","permissionMode":"plan"}}`}, Safe: []string{"--agents", `{"a":{"tools":["permissionMode","x"],"b":{},"model":"sonnet","permissionMode":"plan"}}`}}},
		{name: "grok --print is --single, and takes its prompt", p: gk, args: []string{"--print", "x"},
			want: adapter.ParsedFlags{Print: true, Extra: []string{"--print", "x"}, Safe: []string{"--print", "x"}}},
		{name: "grok --load resumes around the index", p: gk, args: []string{"--load", "x"}, refused: "--load clashes"},
		{name: "grok --append-system-prompt is --rules", p: gk, args: []string{"--append-system-prompt", "x"},
			want: adapter.ParsedFlags{Extra: []string{"--append-system-prompt", "x"}, Safe: []string{"--append-system-prompt", "x"}}},
		{name: "grok --installer writes config", p: gk, args: []string{"--installer", "x"}, refused: "--installer clashes"},
		{name: "grok a hidden value flag", p: gk, args: []string{"--storage-mode", "x"},
			want: adapter.ParsedFlags{Extra: []string{"--storage-mode", "x"}, Safe: []string{"--storage-mode", "x"}}},
		{name: "grok -c is aggy's continue", p: gk, args: []string{"-c"}, want: adapter.ParsedFlags{Continue: true}},
		{name: "grok -m is aggy's model", p: gk, args: []string{"-m", "grok-x"}, want: adapter.ParsedFlags{Model: "grok-x"}},
		{name: "grok -s", p: gk, args: []string{"-s", "x"}, refused: "-s clashes"},
		{name: "grok --cwd", p: gk, args: []string{"--cwd", "/x"}, refused: "--cwd clashes"},
		{name: "grok -w runs it directly", p: gk, args: []string{"-w"},
			want: adapter.ParsedFlags{Print: true, Extra: []string{"-w"}, Safe: []string{"-w"}}},
		{name: "grok --prompt-json", p: gk, args: []string{"--prompt-json", "[]"},
			want: adapter.ParsedFlags{Print: true, Extra: []string{"--prompt-json", "[]"}, Safe: []string{"--prompt-json", "[]"}}},
		{name: "grok --disallowedTools is --deny", p: gk, args: []string{"--disallowedTools", "x"},
			want: adapter.ParsedFlags{Extra: []string{"--disallowedTools", "x"}, Safe: []string{"--disallowedTools", "x"}}},
		{name: "grok --permission-mode default", p: gk, args: []string{"--permission-mode", "default"},
			want: adapter.ParsedFlags{Extra: []string{"--permission-mode", "default"}, Safe: []string{"--permission-mode", "default"}}},
		{name: "grok a value flag from its help", p: gk, args: []string{"--add-dir", "/x", "--effort", "high"},
			want: adapter.ParsedFlags{Extra: []string{"--add-dir", "/x", "--effort", "high"}, Safe: []string{"--add-dir", "/x", "--effort", "high"}}},
		{name: "codex --continue", p: cx, args: []string{"--continue"}, want: adapter.ParsedFlags{Continue: true}},

		{name: "a -c needs a value to classify", p: cx, args: []string{"-c", "model_verbosity"}, loosens: true},
		{name: "a value flag takes a word", p: cl, args: []string{"--add-dir", "/x"},
			want: adapter.ParsedFlags{Extra: []string{"--add-dir", "/x"}, Safe: []string{"--add-dir", "/x"}}},
		{name: "--flag=value needs no table entry", p: cl, args: []string{"--newflag=x"},
			want: adapter.ParsedFlags{Extra: []string{"--newflag=x"}, Safe: []string{"--newflag=x"}}},

		// T6961: only a flag known to take a value takes the next word; any
		// other bare word is the prompt, passed in place and kept out of Safe.
		{name: "a prompt after a switch", p: cl, args: []string{"--verbose", "fix the bug"},
			want: adapter.ParsedFlags{Extra: []string{"--verbose", "fix the bug"}, Safe: []string{"--verbose"}, Prompt: []string{"fix the bug"}}},
		{name: "a subcommand after a switch", p: cx, args: []string{"--search", "resume", "--last"}, refused: "names a subcommand"},
		{name: "a word after an unknown flag", p: gk, args: []string{"--newflag", "x"},
			want: adapter.ParsedFlags{Extra: []string{"--newflag", "x"}, Safe: []string{"--newflag"}, Prompt: []string{"x"}}},
		{name: "two words after a value flag", p: cl, args: []string{"--effort", "high", "fix"},
			want: adapter.ParsedFlags{Extra: []string{"--effort", "high", "fix"}, Safe: []string{"--effort", "high"}, Prompt: []string{"fix"}}},

		{name: "a prompt", p: cl, args: []string{"fix the bug"},
			want: adapter.ParsedFlags{Extra: []string{"fix the bug"}, Prompt: []string{"fix the bug"}}},
		{name: "a prompt, then flags", p: cl, args: []string{"fix", "--verbose"},
			want: adapter.ParsedFlags{Extra: []string{"fix", "--verbose"}, Safe: []string{"--verbose"}, Prompt: []string{"fix"}}},
		{name: "a subcommand", p: cx, args: []string{"resume", "abc"}, refused: "names a subcommand"},
		{name: "a subcommand of claude", p: cl, args: []string{"mcp", "list"}, refused: "names a subcommand"},
		{name: "a subcommand's name later in a prompt", p: cl, args: []string{"fix", "mcp"},
			want: adapter.ParsedFlags{Extra: []string{"fix", "mcp"}, Prompt: []string{"fix", "mcp"}}},
		{name: "a word after a flag's value", p: cl, args: []string{"--permission-mode", "plan", "fix"},
			want: adapter.ParsedFlags{Extra: []string{"--permission-mode", "plan", "fix"}, Safe: []string{"--permission-mode", "plan"}, Prompt: []string{"fix"}}},
		{name: "a word after --flag=value", p: cl, args: []string{"--permission-mode=plan", "fix"},
			want: adapter.ParsedFlags{Extra: []string{"--permission-mode=plan", "fix"}, Safe: []string{"--permission-mode=plan"}, Prompt: []string{"fix"}}},
		{name: "a word after a model", p: cl, args: []string{"--model", "opus", "fix"},
			want: adapter.ParsedFlags{Model: "opus", Extra: []string{"fix"}, Prompt: []string{"fix"}}},
		// Hand-edited for the PR #124 re-review: this row was "-- mcp", which
		// is now refused (below); an ordinary word, or one of aggy's own
		// command names, still passes after --.
		{name: "--", p: cl, args: []string{"--", "sessions", "list"},
			want: adapter.ParsedFlags{Extra: []string{"--", "sessions", "list"}, Prompt: []string{"sessions", "list"}}},
		{name: "-- with a flag-like word that is harmless", p: cl, args: []string{"--", "fix", "--verbose", "-x"},
			want: adapter.ParsedFlags{Extra: []string{"--", "fix", "--verbose", "-x"}, Prompt: []string{"fix", "--verbose", "-x"}}},
		// PR #124 re-review: nothing after -- is left to the CLI's parser.
		{name: "-- then a subcommand", p: cl, args: []string{"--", "mcp"}, refused: "names a subcommand"},
		{name: "-- then a subcommand in another case", p: cl, args: []string{"--", "AGENTS"}, refused: "names a subcommand"},
		{name: "-- then a management subcommand", p: cx, args: []string{"--", "login"}, refused: "names a subcommand"},
		{name: "-- then a grok subcommand", p: gk, args: []string{"--", "Doctor"}, refused: "names a subcommand"},
		{name: "-- then a bypass", p: cl, args: []string{"--", "fix", "--dangerously-skip-permissions"}, loosens: true},
		// Hand-edited for the second re-review: these four rows wanted
		// ErrLoosens or a clash; a flag aggy owns is now refused after --
		// outright, --no-policy or not (Owned).
		{name: "-- then a gated mode", p: cl, args: []string{"--", "fix", "--permission-mode", "bypassPermissions"}, refused: "may not come after --"},
		{name: "-- then a gated mode, = form", p: cl, args: []string{"--", "fix", "--permission-mode=bypassPermissions"}, refused: "may not come after --"},
		{name: "-- then codex -c", p: cx, args: []string{"--", "fix", "-c", "sandbox_mode=danger-full-access"}, refused: "may not come after --"},
		{name: "-- then codex --yolo", p: cx, args: []string{"--", "fix", "--yolo"}, loosens: true},
		{name: "-- then grok --always-approve", p: gk, args: []string{"--", "fix", "--always-approve"}, loosens: true},
		{name: "-- then a refused flag", p: gk, args: []string{"--", "fix", "--cwd", "/x"}, refused: "may not come after --"},
		{name: "-- then a bypass with --no-policy", p: cl, args: []string{"--", "fix", "--dangerously-skip-permissions"}, noPolicy: true,
			want: adapter.ParsedFlags{Extra: []string{"--", "fix", "--dangerously-skip-permissions"}, Prompt: []string{"fix", "--dangerously-skip-permissions"}}},
		{name: "-", p: cl, args: []string{"-"},
			want: adapter.ParsedFlags{Extra: []string{"-"}, Prompt: []string{"-"}}},
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
				got.ResumeID != tt.want.ResumeID || !slices.Equal(got.Extra, tt.want.Extra) || !slices.Equal(got.Safe, tt.want.Safe) ||
				!slices.Equal(got.Prompt, tt.want.Prompt) || got.Print != tt.want.Print {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestParsePrint (T6961): in print mode aggy records no session, so it takes
// over no flag and refuses only what clashes with more than that session.
func TestParsePrint(t *testing.T) {
	cl, cx, gk := claude.Adapter{}.PassFlags(), codex.Adapter{}.PassFlags(), grok.New().PassFlags()
	tests := []struct {
		name    string
		p       adapter.PassFlags
		args    []string
		want    []string
		refused string
	}{
		{"claude's own continue and model", cl, []string{"-c", "-p", "--model", "opus", "check types"}, []string{"-c", "-p", "--model", "opus", "check types"}, ""},
		{"claude's own resume", cl, []string{"-p", "-r", "auth-refactor", "finish"}, []string{"-p", "-r", "auth-refactor", "finish"}, ""},
		{"a session id is the CLI's", cl, []string{"-p", "--session-id", "x"}, []string{"-p", "--session-id", "x"}, ""},
		// PR #124 review: refused in print mode too (claude -p ultrareview).
		{"a subcommand's name is refused", cl, []string{"-p", "ultrareview"}, nil, "names a subcommand"},
		// Hand-edited for the PR #124 re-review: "-p -- mcp" was a prompt; a
		// subcommand's name after -- is now refused in print mode too.
		{"after --, it is a prompt", cl, []string{"-p", "--", "fix"}, []string{"-p", "--", "fix"}, ""},
		{"after --, a subcommand is refused", cl, []string{"-p", "--", "mcp", "serve"}, nil, "names a subcommand"},
		{"after --, a bypass still needs --no-policy", cl, []string{"-p", "--", "x", "--dangerously-skip-permissions"}, nil, "--no-policy"},
		{"codex exec --skip-git-repo-check", cx, []string{"exec", "--skip-git-repo-check", "x"}, nil, "--no-policy"},
		{"codex exec --ignore-user-config", cx, []string{"exec", "--ignore-user-config", "x"}, nil, "--no-policy"},
		{"grok --no-ask-user", gk, []string{"-p", "hi", "--no-ask-user"}, nil, "--no-policy"},
		{"grok --fs-write", gk, []string{"-p", "hi", "--fs-write", "/"}, nil, "--no-policy"},
		{"codex exec's own resume", cx, []string{"exec", "resume", "--last"}, []string{"exec", "resume", "--last"}, ""},
		{"grok -p takes its prompt", gk, []string{"-p", "hi", "--no-alt-screen"}, []string{"-p", "hi", "--no-alt-screen"}, ""},
		{"a bypass still needs --no-policy", cl, []string{"-p", "--dangerously-skip-permissions", "x"}, nil, "--no-policy"},
		{"a session flag of aggy's still clashes on screen only", gk, []string{"-p", "x", "-s", "y"}, []string{"-p", "x", "-s", "y"}, ""},
		{"a cwd still clashes", gk, []string{"-p", "x", "--cwd", "/y"}, nil, "--cwd clashes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Hand-edited for the second re-review: -p as Claude's help shows
			// it, a switch, so a "--" may follow it (rule A).
			got, err := tt.p.ParseWith(tt.args, adapter.ParseOpts{Print: true, Switches: []string{"-p"}})
			if tt.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tt.refused) {
					t.Fatalf("err = %v, want %q", err, tt.refused)
				}
				return
			}
			if err != nil || !slices.Equal(got.Extra, tt.want) {
				t.Errorf("extra %q, err %v; want %q", got.Extra, err, tt.want)
			}
		})
	}
}

// TestRunMode (T6961, T6962): a print flag anywhere but in a flag's value or
// after "--" asks for print mode, as does a print subcommand first; a flag that
// starts no local session runs the CLI as given.
func TestRunMode(t *testing.T) {
	cl, cx, gk := claude.Adapter{}.PassFlags(), codex.Adapter{}.PassFlags(), grok.New().PassFlags()
	const screen, print, bare = adapter.RunScreen, adapter.RunPrint, adapter.RunBare
	// Hand-edited for the PR #124 review: a bare run is a version flag alone,
	// clusters are not split, and the rows the review found run as given now
	// get the policy (screen or print).
	tests := []struct {
		name string
		p    adapter.PassFlags
		args []string
		want adapter.RunMode
	}{
		{"claude -p", cl, []string{"-p", "hi"}, print},
		{"claude --print later", cl, []string{"--model", "opus", "--print"}, print},
		{"claude a cluster is not split", cl, []string{"-dp"}, screen},
		{"claude a value that says -p still adds the policy", cl, []string{"--model", "-p"}, print},
		{"claude after --", cl, []string{"--", "-p"}, screen},
		{"claude none", cl, []string{"--verbose", "hi"}, screen},
		{"claude --teleport", cl, []string{"--teleport"}, print},
		{"claude --version alone", cl, []string{"--version"}, bare},
		{"claude -v alone", cl, []string{"-v"}, bare},
		{"claude --version with -p", cl, []string{"-p", "--version"}, print},
		{"claude -nv", cl, []string{"-nv", "do it"}, screen},
		{"claude -wv", cl, []string{"-wv", "do it"}, screen},
		{"claude a list then --version", cl, []string{"--allowedTools", "--version", "do it"}, screen},
		{"claude --add-dir -v", cl, []string{"--add-dir", "-v", "do it"}, screen},
		{"claude --version and a prompt", cl, []string{"--version", "do it"}, screen},
		{"claude -w alone stays on screen", cl, []string{"-w", "feature"}, screen},
		{"codex -p is a profile", cx, []string{"-p", "work"}, screen},
		{"codex exec", cx, []string{"exec", "hi"}, print},
		{"codex e", cx, []string{"e", "hi"}, print},
		{"codex exec as a later word", cx, []string{"--search", "exec"}, screen},
		{"codex -mV", cx, []string{"-mV", "do it"}, screen},
		{"codex --worktree", cx, []string{"--worktree"}, print},
		{"grok --prompt-file", gk, []string{"--prompt-file", "/x"}, print},
		{"grok -w", gk, []string{"-w"}, print},
		{"grok -v alone", gk, []string{"-v"}, bare},
		{"grok -pv", gk, []string{"-pv"}, screen},
		{"grok -mv", gk, []string{"-mv", "do it"}, screen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.Mode(tt.args); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTakeFlag (T6962): a flag's values come out in either spelling, and
// nothing after "--" is touched.
// TestGated (PR #124 review): a subcommand aggy runs as typed gets no policy,
// so a flag the table gates anywhere in its words is refused outright, and one
// of the extra names too; a flag that clashes only with aggy's session passes.
func TestGated(t *testing.T) {
	cl, cx, gk := claude.Adapter{}.PassFlags(), codex.Adapter{}.PassFlags(), grok.New().PassFlags()
	tests := []struct {
		name  string
		p     adapter.PassFlags
		words []string
		gated bool
	}{
		{"claude mcp list", cl, []string{"list"}, false},
		{"claude a bypass mode", cl, []string{"--permission-mode", "bypassPermissions"}, true},
		{"claude a skip flag", cl, []string{"status", "--dangerously-skip-permissions"}, true},
		{"claude --settings", cl, []string{"--settings", "{}"}, true},
		{"claude a session id passes", cl, []string{"--session-id", "x"}, false},
		// Hand-edited for the PR #124 re-review (rule B): this row wanted
		// false, since words after -- were not looked at; they are now.
		{"claude after -- is still checked", cl, []string{"add", "x", "--", "npx", "--dangerously-x"}, true},
		{"claude a harmless server command after --", cl, []string{"add", "x", "--", "npx", "-y", "pkg@1.0.0"}, false},
		{"claude --settings after a value-taking flag and --", cl, []string{"add", "--transport", "--", "--settings", "{}"}, true},
		{"codex a loose -c", cx, []string{"-c", `sandbox_mode="danger-full-access"`}, true},
		{"codex --yolo in a cluster", cx, []string{"--yolo"}, true},
		{"grok --always-approve", gk, []string{"--always-approve"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.p.Gated(tt.words, "--settings")
			if (err != nil) != tt.gated || err != nil && !errors.Is(err, adapter.ErrSubcommandGated) {
				t.Errorf("Gated = %v, want gated %v", err, tt.gated)
			}
		})
	}
}

// TestDashesEndOptions (PR #124 second re-review): aggy and the CLI's parser
// must agree on where the options end. Rule A: a "--" counts only after words
// that leave no flag waiting for a value; rule B: the words after it are
// checked as flags anyway, and the flags aggy owns are refused there.
func TestDashesEndOptions(t *testing.T) {
	cl, cx, gk := claude.Adapter{}.PassFlags(), codex.Adapter{}.PassFlags(), grok.New().PassFlags()
	sw := []string{"-p", "--print", "--verbose", "-c", "--continue"} // as Claude's help shows them
	bypass := `{"permissions":{"defaultMode":"bypassPermissions"}}`
	tests := []struct {
		name  string
		p     adapter.PassFlags
		args  []string
		print bool
		err   error // nil: parsed
	}{
		// A: a "--" the CLI may take as a value.
		{"--name --", cl, []string{"--name", "--", "--settings", bypass, "do it"}, false, adapter.ErrDashesValue},
		{"-n --", cl, []string{"-n", "--", "--verbose", "agents"}, false, adapter.ErrDashesValue},
		{"print --debug-file --", cl, []string{"-p", "--debug-file", "--", "--settings", bypass, "do it"}, true, adapter.ErrDashesValue},
		{"a list flag --", cl, []string{"--add-dir", "a", "b", "--", "hi"}, false, adapter.ErrDashesValue},
		{"an unknown flag --", cl, []string{"--newflag", "--", "hi"}, false, adapter.ErrDashesValue},
		{"a flag help does not show --", cl, []string{"--debug", "--", "hi"}, false, adapter.ErrDashesValue},
		{"a cluster --", cl, []string{"-pv", "--", "hi"}, true, adapter.ErrDashesValue},
		{"a resume with no id --", cl, []string{"-p", "-r", "--", "hi"}, true, adapter.ErrDashesValue},
		{"a prompt word --", cl, []string{"fix", "--", "more"}, false, adapter.ErrDashesValue},
		{"a checked flag takes --", cl, []string{"--permission-mode", "--", "hi"}, false, adapter.ErrDashesValue},
		{"a dir flag takes --", cx, []string{"--add-dir", "--", "--settings", "x"}, false, adapter.ErrDashesValue},
		{"codex exec's own flag --", cx, []string{"exec", "--json", "--", "hi"}, true, adapter.ErrDashesValue},
		{"grok -p takes --", gk, []string{"-p", "--", "--always-approve"}, true, adapter.ErrDashesValue},
		{"a known switch --", cl, []string{"--verbose", "--", "hi"}, false, nil},
		{"print -p --", cl, []string{"-p", "--", "hi"}, true, nil},
		{"--flag=value --", cl, []string{"--name=x", "--", "hi"}, false, nil},
		{"a value flag and its value --", cl, []string{"--name", "x", "--", "hi"}, false, nil},
		{"a model --", cl, []string{"--model", "opus", "--", "hi"}, false, nil},
		{"a resume id --", cl, []string{"-p", "-r", "abc", "--", "hi"}, true, nil},
		{"codex exec --", cx, []string{"exec", "--", "hi"}, true, nil},
		{"-- first", cl, []string{"--", "sessions"}, false, nil},
		// B: after "--", checked anyway.
		{"-- --settings", cl, []string{"--", "--settings", bypass}, false, adapter.ErrOwnedAfterDashes},
		{"-- --settings=", cl, []string{"--", "x", "--settings=" + bypass}, true, adapter.ErrOwnedAfterDashes},
		{"-- --mcp-config", cl, []string{"--", "x", "--mcp-config", "m.json"}, false, adapter.ErrOwnedAfterDashes},
		{"-- --append-system-prompt-file", cl, []string{"--", "x", "--append-system-prompt-file=f"}, false, adapter.ErrOwnedAfterDashes},
		{"-- --allowedTools", cl, []string{"--", "x", "--allowedTools", "Bash"}, false, adapter.ErrOwnedAfterDashes},
		{"-- --model", cl, []string{"--", "x", "--model", "opus"}, false, adapter.ErrOwnedAfterDashes},
		{"-- a cluster with -c", cl, []string{"--", "x", "-vc"}, false, adapter.ErrOwnedAfterDashes},
		{"-- codex -c", cx, []string{"--", "x", "-cfoo=1"}, false, adapter.ErrOwnedAfterDashes},
		{"-- codex -s", cx, []string{"--", "x", "-s", "read-only"}, true, adapter.ErrOwnedAfterDashes},
		{"-- grok --rules", gk, []string{"--", "x", "--rules=y"}, true, adapter.ErrOwnedAfterDashes},
		{"-- grok --deny", gk, []string{"--", "x", "--deny", "y"}, false, adapter.ErrOwnedAfterDashes},
		{"-- grok --agent", gk, []string{"--", "x", "--agent", "y"}, false, adapter.ErrOwnedAfterDashes},
		{"-- a flag then a subcommand", cl, []string{"--", "--verbose", "agents"}, false, adapter.ErrCommandAfterDashes},
		{"-- a blank word then a subcommand", cl, []string{"--", "", "agents"}, false, adapter.ErrCommandAfterDashes},
		{"-- a value then a subcommand", cl, []string{"--", "--name", "x", "AGENTS"}, false, adapter.ErrCommandAfterDashes},
		{"-- a bypass", cl, []string{"--", "x", "--dangerously-skip-permissions"}, false, adapter.ErrLoosens},
		{"-- a prompt naming a subcommand later", cl, []string{"--", "fix", "agents"}, false, nil},
		{"-- an ordinary flag-like word", cl, []string{"--", "fix", "--verbose"}, false, nil},
		// Before "--": a word the CLI may take as a value is still checked as a flag.
		{"a gated flag as a value", cl, []string{"--name", "--dangerously-skip-permissions"}, false, adapter.ErrLoosens},
		{"a blank word then a subcommand", cl, []string{"", "agents"}, false, adapter.ErrNotFlag},
		{"a subcommand in another case", cl, []string{"Agents"}, false, adapter.ErrNotFlag},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.p.ParseWith(tt.args, adapter.ParseOpts{Print: tt.print, Switches: sw})
			if tt.err == nil && err != nil || tt.err != nil && !errors.Is(err, tt.err) {
				t.Errorf("err = %v, want %v", err, tt.err)
			}
		})
	}
}

func TestTakeFlag(t *testing.T) {
	// Hand-edited for the PR #124 re-review (rule C): TakeFlag no longer stops
	// at "--", so the last --settings is taken too (it used to stay in rest).
	vals, rest := adapter.TakeFlag([]string{"--settings", "a.json", "-p", "--settings={}", "hi", "--", "--settings", "x"}, "--settings")
	if !slices.Equal(vals, []string{"a.json", "{}", "x"}) || !slices.Equal(rest, []string{"-p", "hi", "--"}) {
		t.Errorf("vals %q, rest %q", vals, rest)
	}
	// Every occurrence, both forms, and one after a flag aggy cannot tell
	// takes a value.
	vals, rest = adapter.TakeFlag([]string{"--unknown", "--settings", "a", "--name", "--settings=b", "--settings", "c"}, "--settings")
	if !slices.Equal(vals, []string{"a", "b", "c"}) || !slices.Equal(rest, []string{"--unknown", "--name"}) {
		t.Errorf("vals %q, rest %q", vals, rest)
	}
	if vals, rest := adapter.TakeFlag([]string{"x", "--rules"}, "--rules"); len(vals) != 0 || !slices.Equal(rest, []string{"x", "--rules"}) {
		t.Errorf("a flag with no value: vals %q, rest %q", vals, rest)
	}
}

// TestPassFlagsNotes (T6952): a flag worth a line at launch passes and asks for
// it, once.
func TestPassFlagsNotes(t *testing.T) {
	cl, cx, gk := claude.Adapter{}.PassFlags(), codex.Adapter{}.PassFlags(), grok.New().PassFlags()
	tests := []struct {
		name string
		p    adapter.PassFlags
		args []string
		want string
	}{
		{"codex --add-dir", cx, []string{"--add-dir", "/srv/a", "--add-dir=/srv/b"}, "more folders"},
		{"grok --add-dir", gk, []string{"--add-dir", "/srv/a"}, "more folders"},
		{"codex --profile", cx, []string{"--profile", "x"}, "layers your own config"},
		{"codex -p", cx, []string{"-px"}, "layers your own config"},
		{"codex --enable", cx, []string{"--enable", "x"}, "turns on a Codex feature"},
		{"claude --safe-mode", cl, []string{"--safe-mode"}, "hooks"},
		{"claude --bare", cl, []string{"--bare"}, "hooks"},
		{"nothing to say", cl, []string{"--verbose"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.p.Parse(tt.args, false)
			if err != nil {
				t.Fatal(err)
			}
			if tt.want == "" {
				if len(got.Notes) != 0 {
					t.Errorf("notes = %q", got.Notes)
				}
				return
			}
			if len(got.Notes) != 1 || !strings.Contains(got.Notes[0], tt.want) || !slices.Equal(got.Extra, tt.args) {
				t.Errorf("got %+v, want one note with %q", got, tt.want)
			}
		})
	}
}

// TestPassFlagsDirs (T6952): a folder flag's value is listed as typed, in any
// spelling, for the launch to resolve; a flag that names no folder is not.
func TestPassFlagsDirs(t *testing.T) {
	cl, cx, gk := claude.Adapter{}.PassFlags(), codex.Adapter{}.PassFlags(), grok.New().PassFlags()
	tests := []struct {
		name string
		p    adapter.PassFlags
		args []string
		want []adapter.DirFlag
	}{
		{"codex two words", cx, []string{"--add-dir", "/x"}, []adapter.DirFlag{{Name: "--add-dir", Value: "/x"}}},
		{"codex with an =", cx, []string{"--add-dir=../y"}, []adapter.DirFlag{{Name: "--add-dir", Value: "../y"}}},
		{"grok", gk, []string{"--add-dir", "/x"}, []adapter.DirFlag{{Name: "--add-dir", Value: "/x"}}},
		{"claude's permission layer decides", cl, []string{"--add-dir", "/x"}, nil},
		{"another flag", cx, []string{"--image", "/x"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.p.Parse(tt.args, false)
			if err != nil || !slices.Equal(got.Dirs, tt.want) {
				t.Errorf("dirs %v, err %v; want %v", got.Dirs, err, tt.want)
			}
		})
	}
}
