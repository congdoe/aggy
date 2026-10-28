// Package cli parses aggy's command line and dispatches to commands.
package cli

import (
	"fmt"
	"io"
	"runtime/debug"
)

// Version is set at build time:
//
//	go build -ldflags "-X github.com/congdoe/aggy/internal/cli.Version=v0.1.0"
var Version = "dev"

// version is Version, or, in a binary built by `go install
// github.com/congdoe/aggy/cmd/aggy@<tag>` (no ldflags), the module
// version Go recorded.
func version() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return Version
	}
	return pickVersion(Version, bi.Main.Version)
}

func pickVersion(set, module string) string {
	if set != "dev" || module == "" || module == "(devel)" {
		return set
	}
	return module
}

const usage = `aggy runs the official Claude Code, Codex, and Grok Build CLIs.

Usage:
  aggy <command>

Commands:
  borrow    Run one bounded task headless in another vendor's CLI
  image     Make an image in another vendor's CLI and copy it into the repo
  video     Make a short video in Grok's CLI and copy it into the repo
  inbox     List unread borrow reports, or show one
  run       Start a vendor CLI (claude, codex, or grok) under aggy; with
            --direct, on your terminal without aggy's pass-through (for
            when it breaks a terminal feature; no switch key), still
            kept in the index through hooks. aggy --direct V is the same.
  brief     Preview the handoff brief for a vendor session
  setup     Check each vendor CLI's install, version, and sign-in
  doctor    Check the vendor CLIs, API-key and proxy settings, and config drift
  log       Show harnesses, models, switches, borrows, and files changed
  unwire    Undo the config changes aggy made to other tools
  skill     Link aggy's skill into each vendor CLI, or unlink it
  hooks     Install the vendor hooks aggy listens to, or show them
  version   Print the aggy version
  help      Show this help

aggy is pre-alpha. Nothing else is implemented yet.
`

// Run executes aggy with args, which exclude the program name, and returns
// the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "aggy %s\n", version())
		return 0
	case "borrow":
		return runBorrow(args[1:], stdout, stderr)
	case "image":
		return runImage(args[1:], stdout, stderr)
	case "video":
		return runVideo(args[1:], stdout, stderr)
	case "inbox":
		return runInbox(args[1:], stdout, stderr)
	case "brief":
		return runBrief(args[1:], stdout, stderr)
	case "hook":
		return runHook(args[1:], stdout, stderr)
	case "run":
		return runRun(args[1:], stdout, stderr)
	case "--direct":
		return runRun(args, stdout, stderr)
	case "setup":
		return runSetup(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "log":
		return runLog(args[1:], stdout, stderr)
	case "dev":
		return runDev(args[1:], stdout, stderr)
	case "unwire":
		return runUnwire(args[1:], stdout, stderr)
	case "skill":
		return runSkill(args[1:], stdout, stderr)
	case "hooks":
		return runHooks(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "aggy: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
