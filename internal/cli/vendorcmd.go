package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/adapter/opencode"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/termsafe"
)

const vendorUsage = `Usage: aggy <claude|codex|grok|opencode> [--direct] [--no-policy] [--tool NAME]... [the CLI's own flags] [prompt]

Starts a new session of that CLI here, with the CLI's own flags and first
prompt, as if you had opened it yourself: aggy claude --model opus "fix the
build". Without a vendor, aggy [flags] [prompt] is the default vendor's.

  --continue    Reopen the latest session of this CLI here, or refuse (also
                -c on claude)
  --resume [ID] Reopen the aggy session with this id, or the latest here
                (also -r on claude and grok)
  --model NAME  The model for this session (also -m on codex)
  --direct      On your terminal, without aggy's pass-through (no switch key)
  --no-policy   Without aggy's safety policy
  --tool NAME   Turn a tool on for this session only

Every other word goes to the CLI, in the order given, after aggy's own launch
flags. A flag aggy passes too is merged with aggy's own: claude --settings,
--mcp-config, and --append-system-prompt-file, and grok --rules. aggy refuses
the ones that clash with the session it records (--session-id and so on) or
run it outside this terminal (claude --desktop, --cloud; the message says
why). A flag whose session aggy cannot record (claude --teleport; codex and
grok --worktree) runs the CLI as print mode does. Only --version, as the one
word, runs as given. A bare word directly after a flag that takes a value (the
CLI's help says which) is that value; any other is the prompt, and the session
starts with it.

A management subcommand that runs no agent runs as typed, with nothing of
aggy's added and no session recorded: aggy claude mcp list, aggy claude auth
login, aggy codex login, aggy grok update, aggy opencode models. Its --help is
the CLI's. A flag that loosens the policy is refused there. An IDE
panel, the desktop app, or a web session starts the CLI without aggy, so
aggy's policy and memory do not apply there. A first prompt word that names one of the CLI's subcommands is
refused: put the prompt after --. A prompt with --continue or --resume is
claude's only. OpenCode takes no prompt yet.

Print mode (claude -p, grok -p, codex exec) and a stdin that is no terminal
run the CLI on your terminal with no switch key and no session recorded: aggy
adds its safety policy, your shared rules, and the shared memory, and passes
the rest as typed, -c and -r included; stdin, stdout, and the exit code are
the CLI's. git diff | aggy claude -p "review this".

A flag that could get past aggy's safety policy or hooks (--dangerously-*,
--permission-mode bypassPermissions, --yolo, -a never, --add-dir of your home or
a folder of keys on codex and grok, and on codex --disable hooks and a -c aggy
cannot tell is harmless) needs --no-policy, and aggy notes it at launch. A flag
that only asks you less passes where your deny rules still hold, as on claude
--allowedTools and --permission-mode auto, and some pass with a line at launch,
as on codex --profile and --enable.

aggy opencode takes only the flags above, none of OpenCode's own. Its model
is provider/model. As on the other CLIs, --model on a resume applies in opencode
mini; on the full screen a resumed session keeps its own. Full auto is the
[opencode] auto setting, in a repo you ran aggy trust --opencode on.
`

// passFlags is v's table of flags aggy takes over or refuses.
func passFlags(v adapter.Vendor) adapter.PassFlags {
	return map[adapter.Vendor]adapter.Adapter{adapter.Claude: claude.Adapter{}, adapter.Codex: codex.Adapter{}, adapter.Grok: grok.New(), adapter.OpenCode: opencode.New()}[v].PassFlags()
}

// launchUsageErr prints a launch command's usage error: the tagged line,
// then the usage text.
func launchUsageErr(stderr io.Writer, cmd string, code diag.Code, msg, usage string) int {
	refuse(context.Background(), stderr, cmd, diag.EvLaunchRefused, code, msg, diag.Rec{})
	if usage != "" {
		fmt.Fprintf(stderr, "\n%s", usage)
	}
	return 2
}

// sessionArg checks an id given on the command line, and returns it in
// lower case, or the message that refuses it.
func sessionArg(arg string) (string, string) {
	id := strings.ToLower(arg)
	if len(id) < 4 || len(id) > 36 || strings.Trim(id, "0123456789abcdef-") != "" {
		return "", fmt.Sprintf("%q is not a session id (at least 4 hex digits; aggy sessions lists them)", termsafe.Field(arg))
	}
	return id, ""
}

// gateDirFlags gates a folder flag whose folder would give the session a home
// or keys (safefs.HoldsSecrets). It runs once the plan knows cwd, the directory
// the CLI will start in, which on a resume is the child's and not aggy's, since
// a relative value means that one there. An adapter touches no disk, so this is
// here. With --no-policy the flag goes through as any loosening flag does: noted,
// and kept from a later launch of the session.
func gateDirFlags(t *target, cwd string) error {
	kept := t.Dirs[:0:0]
	for _, d := range t.Dirs {
		why := dirGate(d.Value, cwd)
		if why == "" {
			kept = append(kept, d)
			continue
		}
		if !t.NoPolicy {
			return fmt.Errorf("%s %s %s: %w", d.Name, d.Value, why, adapter.ErrLoosens)
		}
		t.Loosens = append(t.Loosens, d.Name)
		t.SafeExtra = withoutFlag(t.SafeExtra, d)
	}
	t.Dirs = kept
	return nil
}

// dirGate says why a folder flag's value, read against cwd, is gated, or "". A
// value it cannot make a path of, or resolve, is gated too, in its own words.
func dirGate(dir, cwd string) string {
	switch {
	case dir == "~" || strings.HasPrefix(dir, "~/"):
		// The CLI may or may not expand it: both readings are checked.
		home := os.Getenv("HOME")
		if home == "" {
			return "names your home, and HOME is not set, so aggy cannot tell what it reaches"
		}
		if why := dirGate(home+strings.TrimPrefix(dir, "~"), cwd); why != "" {
			return why
		}
	case strings.HasPrefix(dir, "~"):
		return "names another user's home, which aggy does not expand, so it cannot tell what it reaches"
	}
	if !filepath.IsAbs(dir) {
		// Not Join: a ".." keeps its physical meaning through a symlink.
		dir = cwd + string(filepath.Separator) + dir
	}
	held, err := safefs.HoldsSecrets(dir)
	switch {
	case err != nil:
		return "could not be resolved (a symlink loop, or a path aggy may not read), so aggy cannot tell what it reaches"
	case held:
		return "is, holds, or is inside your home's keys or a vendor's or aggy's config"
	}
	return ""
}

// withoutFlag is words without d, written as two words or with an =.
func withoutFlag(words []string, d adapter.DirFlag) []string {
	var out []string
	for i := 0; i < len(words); i++ {
		switch {
		case words[i] == d.Name+"="+d.Value:
		case words[i] == d.Name && i+1 < len(words) && words[i+1] == d.Value:
			i++
		default:
			out = append(out, words[i])
		}
	}
	return out
}
