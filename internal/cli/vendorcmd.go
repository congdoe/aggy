package cli

import (
	"context"
	"errors"
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
	"github.com/congdoe/aggy/internal/tooldef"
)

const vendorUsage = `Usage: aggy <claude|codex|grok|opencode> [--direct] [--no-policy] [--tool NAME]... [the CLI's own flags]

Starts a new session of that CLI here, with the CLI's own flags, as if you
had opened it yourself: aggy claude --model opus --permission-mode plan.

  --continue    Reopen the latest session of this CLI here, or refuse (also
                -c on claude)
  --resume [ID] Reopen the aggy session with this id, or the latest here
                (also -r on claude and grok)
  --model NAME  The model for this session (also -m on codex)
  --direct      On your terminal, without aggy's pass-through (no switch key)
  --no-policy   Without aggy's safety policy
  --tool NAME   Turn a tool on for this session only

Every other flag goes to the CLI. aggy refuses the ones that clash with what
it passes at launch (--session-id, --settings, and so on; the message says
why), a prompt, and a subcommand: type the prompt once the session starts,
and run the CLI itself for its subcommands and its own --help.

A flag that could get past aggy's safety policy or hooks (--dangerously-*,
--permission-mode bypassPermissions, --yolo, -a never, --add-dir of your home or
a folder of keys on codex and grok, and on codex --disable hooks and a -c aggy
cannot tell is harmless) needs --no-policy, and aggy notes it at launch. A flag
that only asks you less passes where your deny rules still hold, as on claude
--allowedTools and --permission-mode auto, and some pass with a line at launch,
as on codex --profile and --enable. A bare word is taken as a value only after a
flag aggy knows takes one; for any other flag write --flag=value.

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

// runVendor is `aggy <vendor> [flags]`: a new session of v here, or with
// --continue or --resume an old one, with the CLI's own flags for it. Ids
// never come from these flags: --resume takes an aggy id, checked as aggy
// resume checks it, and the adapter passes only the id the index holds.
func runVendor(v adapter.Vendor, args []string, stderr io.Writer) int {
	name := string(v)
	t := target{Vendor: v}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		flag, val, hasVal := strings.Cut(a, "=")
		switch {
		case a == "--direct":
			t.Direct = true
		case a == "--no-policy":
			t.NoPolicy = true
		case flag == "--tool":
			if !hasVal {
				if i+1 == len(args) {
					return launchUsageErr(stderr, name, diag.CodeUsage, "--tool needs a name", vendorUsage)
				}
				i++
				val = args[i]
			}
			if !tooldef.ValidName(val) {
				return launchUsageErr(stderr, name, diag.CodeUsage, fmt.Sprintf("%q is not a tool name", val), "")
			}
			t.Tools = append(t.Tools, val)
		default:
			rest = append(rest, a)
		}
	}
	p, err := passFlags(v).Parse(rest, t.NoPolicy)
	if err != nil {
		code := diag.CodeFlagClash
		switch {
		case errors.Is(err, adapter.ErrNotFlag):
			code = diag.CodeUsage
		case errors.Is(err, adapter.ErrLoosens):
			code = diag.CodeFlagLoosens
		}
		return launchUsageErr(stderr, name, code, err.Error(), "")
	}
	if v == adapter.OpenCode && len(p.Extra) > 0 {
		return launchUsageErr(stderr, name, diag.CodeFlagClash, fmt.Sprintf("aggy passes none of OpenCode's own flags yet, so not %q", p.Extra[0]), vendorUsage)
	}
	t.Model, t.Extra, t.SafeExtra, t.Loosens, t.FlagNotes, t.Dirs = p.Model, p.Extra, p.Safe, p.Loosens, p.Notes, p.Dirs
	switch {
	case p.Continue && p.Resume:
		return launchUsageErr(stderr, name, diag.CodeUsage, "use --continue or --resume, not both", vendorUsage)
	case p.Continue:
		t.Continue = true
	case p.Resume && p.ResumeID != "":
		id, msg := sessionArg(p.ResumeID)
		if msg != "" {
			return launchUsageErr(stderr, name, diag.CodeUsage, msg, "")
		}
		t.ParentID = id
	case p.Resume:
		t.Continue = true
	default:
		t.New = true
	}
	return launch("aggy "+name, t, stderr)
}

// runContinue is `aggy --continue` (-c): the session last active here, in
// any vendor; an error when there is none, as for claude --continue.
func runContinue(args []string, stderr io.Writer) int {
	t := target{Continue: true}
	for _, a := range args {
		switch a {
		case "--no-policy":
			t.NoPolicy = true
		case "--continue", "-c":
		default:
			return cmdUsage(stderr, "", "aggy --continue takes only --no-policy", continueUsage)
		}
	}
	return launch("aggy", t, stderr)
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
