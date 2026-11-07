package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
)

const vendorUsage = `Usage: aggy <claude|codex|grok> [--direct] [--no-policy] [--tool NAME]... [the CLI's own flags]

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
`

// passFlags is v's table of flags aggy takes over or refuses.
func passFlags(v adapter.Vendor) adapter.PassFlags {
	return map[adapter.Vendor]adapter.Adapter{adapter.Claude: claude.Adapter{}, adapter.Codex: codex.Adapter{}, adapter.Grok: grok.New()}[v].PassFlags()
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
func runVendor(v adapter.Vendor, args []string, stdout, stderr io.Writer) int {
	name := string(v)
	t := target{Vendor: v}
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		flag, val, hasVal := strings.Cut(a, "=")
		switch {
		case a == "--help" || a == "-h":
			fmt.Fprint(stdout, vendorUsage)
			return 0
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
		return launchUsageErr(stderr, name, diag.CodeFlagClash, err.Error(), "")
	}
	t.Model, t.Extra = p.Model, p.Extra
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
			fmt.Fprint(stderr, "usage: aggy --continue [--no-policy]\n")
			return 2
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
