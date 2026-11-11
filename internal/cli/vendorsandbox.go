package cli

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/trustlist"
)

// The vendors' own sandboxes on screen (T6980, docs/design.md section 10,
// "The vendors' own sandboxes"). aggy builds no OS sandbox of its own: it
// turns Claude Code's on, per folder, through the trust list and the inline
// --settings layer it already passes (claude.SandboxLayer; argv, never a
// write to Claude's settings files), and it shows Codex's, which is on by
// default, and asks before a flag that turns it off (codex.SandboxLevel).
// The facts behind both are in the adapters' sandbox.go, from the vendors'
// docs and source, not live-checked.

// trustRowClaudeSandbox is the trust list's row for Claude's sandbox.
const trustRowClaudeSandbox = "claude-sandbox"

// claudeSandboxWarning is the row's line when the sandbox can start here:
// what it confines, and what it costs in daily use (PR #135 review).
const claudeSandboxWarning = "its shell commands may then write only in the folder it starts in and a temp folder, and get no network unless a domain is allowed, " +
	"so git push or npm install will raise Claude's own \"run outside the sandbox?\" prompt; Claude still asks before each command as it does now, " +
	"and its file tools, hooks, and MCP servers stay outside it (Claude Code's docs)"

func init() {
	registerTrustRow(trustRowDef{
		Row: trustlist.Row{ID: trustRowClaudeSandbox, Label: "Claude Code: run its shell commands in Claude's own sandbox", Group: trustlist.Setting,
			Warning: claudeSandboxWarning, Quiet: true},
		bin: vendorBin(adapter.Claude), order: 10,
		// Unticked by default, as every row (PR #135 review): it costs
		// network in daily use, so the person chooses it. A folder answered
		// before the row existed is not asked again (Quiet), and it stays off
		// there until aggy trust shows the list.
		note: func() string {
			if m := claudeSandboxMissing(); m != "" {
				return "it cannot start on this machine: " + m + "; ticked, aggy asks before it starts Claude Code without it, and refuses to start it with no one to ask"
			}
			return ""
		},
		// Its tick is the folder's record itself, which the launch reads
		// (claudeSandboxHere).
		plan: func(trustTarget, bool) (trustRowPlan, error) { return trustRowPlan{}, nil },
	})
}

// claudeSandboxMissing says what keeps Claude Code's sandbox from starting
// on this machine, or "" when nothing aggy can see does. From the docs
// (claude/sandbox.go): macOS has Seatbelt built in; Linux and WSL2 need
// bubblewrap (bwrap) and socat; elsewhere commands run unsandboxed. A WSL1
// machine, or a kernel that refuses bubblewrap its namespaces, passes this
// check; Claude then exits at startup (failIfUnavailable). A test swaps it.
var claudeSandboxMissing = func() string {
	switch runtime.GOOS {
	case "darwin":
		return ""
	case "linux":
		var missing []string
		for _, bin := range []string{"bwrap", "socat"} {
			if _, err := probe.LookPath(bin); err != nil {
				missing = append(missing, bin)
			}
		}
		if len(missing) == 0 {
			return ""
		}
		return strings.Join(missing, " and ") + " not found on PATH; Claude Code's sandbox on Linux needs bubblewrap and socat installed"
	}
	return "Claude Code's sandbox runs on macOS, Linux, and WSL2 only, and this is " + runtime.GOOS
}

// claudeSandboxHere reports whether dir's trust list ticks Claude's sandbox,
// and the folder's work-tree root for a line ("" for a folder never offered
// the list: / or the home folder).
func claudeSandboxHere(dir string) (on bool, root string) {
	tg, err := trustTargetFor(dir)
	if err != nil {
		return false, ""
	}
	path, err := trustFile()
	if err != nil {
		return false, tg.root
	}
	f, err := readTrustRecord(path)
	if err != nil {
		return false, tg.root
	}
	return slices.Contains(f.Folders[tg.key].On, trustRowClaudeSandbox), tg.root
}

// errClaudeSandboxDeclined is the launch refused when the person, asked,
// would not start Claude Code without the sandbox they chose.
var errClaudeSandboxDeclined = errors.New("not started: you chose to run Claude Code sandboxed in this folder, and its sandbox cannot start here")

// errClaudeSandboxNoOne is the launch refused when the sandbox the folder
// chose cannot start and no one is at a terminal to say to start without it.
var errClaudeSandboxNoOne = errors.New("not started: this folder chose to run Claude Code sandboxed, its sandbox cannot start here, and no one is at a terminal to say to start without it")

// claudeSandboxLaunch sets req to turn Claude's sandbox on when cwd's trust
// list ticks it, and returns the launch's line saying what holds. noPolicy
// keeps it on: --no-policy turns off aggy's decisions, not confinement the
// person chose (PR #135 review). Ticked but unable to start, it asks in place
// whether to start without it, and a no refuses the launch; with no one to
// ask (no terminal, a borrow child, a session's tool shell) it refuses too,
// naming the ways forward (PR #135 review).
func claudeSandboxLaunch(req *adapter.InteractiveRequest, cwd string, noPolicy bool, getenv func(string) string, in io.Reader, out io.Writer) (string, error) {
	on, root := claudeSandboxHere(cwd)
	if !on {
		switch {
		case noPolicy:
			return "Claude's sandbox: as your own Claude settings set it (--no-policy, and not chosen for this folder)", nil
		case root == "":
			return "Claude's sandbox: aggy does not turn it on in this folder; your own Claude settings decide", nil
		}
		return "Claude's sandbox: not chosen for this folder, so your own Claude settings decide; aggy trust " + root + " offers it", nil
	}
	missing := claudeSandboxMissing()
	if missing == "" {
		req.ClaudeSandbox = true
		line := "Claude's sandbox: on for its shell commands (writes to this folder and temp only, no network unless a domain is allowed), and Claude still asks before each command; " +
			"its file tools, hooks, and MCP servers are outside it, and Claude exits if the sandbox cannot start"
		if noPolicy {
			line = "Claude's sandbox: still on with --no-policy, since this folder chose it; aggy trust " + root + " unticks it"
		}
		return line, nil
	}
	ways := "install what is missing (on Linux, bubblewrap and socat), or untick the sandbox with aggy trust " + root
	yes, asked := askInPlace(getenv, in, out, inPlace{
		change: "You chose to run Claude Code sandboxed in this folder, but its sandbox cannot start here: " + missing + ".",
		undo:   "A yes holds for this launch only; aggy trust " + root + " unticks the choice for good.",
		ask:    "Start Claude Code without its sandbox this time?",
	})
	switch {
	case !asked:
		return "", fmt.Errorf("%w (%s); %s", errClaudeSandboxNoOne, missing, ways)
	case !yes:
		return "", fmt.Errorf("%w (%s); %s", errClaudeSandboxDeclined, missing, ways)
	}
	return "Claude's sandbox: off this launch, as you said: " + missing, nil
}

// codexSandboxLine is a Codex launch's line for its sandbox, from the CLI
// words the person gave.
func codexSandboxLine(extra []string) string {
	return codex.SandboxLine(codex.SandboxLevel(extra))
}

// Doctor's findings for the vendors' sandboxes.
const codeVendorSandbox = "vendor-sandbox"

// claudeSandboxFinding says, for aggy doctor, whether Claude's sandbox is
// chosen for cwd and whether it can start on this machine. Its message holds
// no path.
func claudeSandboxFinding(cwd string) checkup.Finding {
	f := checkup.Finding{Code: codeVendorSandbox, Level: checkup.LevelInfo, Vendor: adapter.Claude}
	on, root := claudeSandboxHere(cwd)
	missing := claudeSandboxMissing()
	switch {
	case on && missing != "":
		f.Level = checkup.LevelWarn
		f.Message = "Claude's sandbox: chosen for this folder, but it cannot start on this machine: aggy asks at a terminal before starting Claude Code without it, and refuses to start it with no one to ask"
		f.Detail = missing
	case on:
		f.Message = "Claude's sandbox: on for this folder's sessions (its shell commands only; aggy passes sandbox.enabled in its --settings layer)"
	case root == "":
		f.Message = "Claude's sandbox: aggy does not offer it in this folder; your own Claude settings decide"
	case missing != "":
		f.Message = "Claude's sandbox: not chosen for this folder, and it cannot start on this machine"
		f.Detail = missing
	default:
		f.Message = "Claude's sandbox: not chosen for this folder (aggy trust offers it); your own Claude settings decide"
	}
	return f
}

// codexSandboxFinding says, for aggy doctor, how aggy treats Codex's sandbox.
func codexSandboxFinding() checkup.Finding {
	return checkup.Finding{Code: codeVendorSandbox, Level: checkup.LevelInfo, Vendor: adapter.Codex,
		Message: "Codex's sandbox: on by default at the level Codex picks (its docs: workspace-write in a version-controlled folder, read-only elsewhere); " +
			"aggy passes --sandbox read-only and workspace-write, asks before danger-full-access, and refuses any other level"}
}
