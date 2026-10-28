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

// version is Version when the build set it with -ldflags (make build,
// goreleaser), else the main module version Go recorded: the tag for
// `go install github.com/congdoe/aggy/cmd/aggy@<tag>`, and, since Go
// 1.24, a VCS pseudo-version for a plain `go build` in a checkout. A
// build with neither (go run, tests) says "dev".
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
  aggy              Resume the latest session in this directory, or start one
  aggy <command>

Commands:
  borrow    Run one bounded task headless in another vendor's CLI
  image     Make an image in another vendor's CLI and copy it into the repo
  video     Make a short video in Grok's CLI and copy it into the repo
  inbox     List unread borrow reports, or show one
  accept    Show an edit borrow's diff and apply it to this checkout
  reject    Discard an edit borrow's worktree and branch
  gc        Remove finished worktrees, closed borrow children, old inbox
            reports, and expired research
  run       Start a vendor CLI (claude, codex, or grok) under aggy; with
            --direct, on your terminal without aggy's pass-through (for
            when it breaks a terminal feature; no switch key), still
            kept in the index through hooks. aggy --direct V is the same.
            aggy refuses to start a CLI its safety policy cannot reach;
            --no-policy (on aggy, run, new, resume) starts it without.
  brief     Preview the handoff brief for a vendor session
  new       Start another session in this directory (optionally: a vendor)
  resume    Reopen the latest session here, or the one with this id (a
            child id from aggy session reopens that child); --brief hands
            the child a brief file, --brief --show previews it
  sessions  List every session: directory, last vendor, last active
  session   List this session's children: vendor, kind, title, model
  setup     Check each vendor CLI's install, version, and sign-in
  doctor    Check the vendor CLIs, API-key and proxy settings, and config drift
  log       Show harnesses, models, switches, borrows, and files changed
  diag      Query aggy's diagnostic log: aggy diag show <id> for what
            happened, aggy diag bundle for an issue, aggy diag codes
  stats     Show borrows per kind and vendor: runs, tokens, time, redo rate
  unwire    Undo the config changes aggy made to other tools
  skill     Link aggy's skill into each vendor CLI, or unlink it
  hooks     Install the vendor hooks aggy listens to, or show them
  memory    Import Claude's memory into the shared .aggy/memory.md
  policy    Wire aggy's safety policy into Codex (Claude and Grok get it per launch)
  mcp       Copy Claude's public MCP servers into Codex and Grok
  attribution
            Install the commit hook that adds Aggy-Vendor and Aggy-Model
            trailers to commits made under aggy
  check     Run the repo's typecheck, lint, and tests from .aggy/config.toml
  version   Print the aggy version
  help      Show this help

aggy is pre-alpha. Nothing else is implemented yet.
`

// Run executes aggy with args, which exclude the program name, and returns
// the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return launch("aggy", target{}, stderr)
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
	case "accept":
		return runAccept(args[1:], stdout, stderr)
	case "reject":
		return runReject(args[1:], stdout, stderr)
	case "brief":
		return runBrief(args[1:], stdout, stderr)
	case "hook":
		return runHook(args[1:], stdout, stderr)
	case "run":
		return runRun(args[1:], stdout, stderr)
	case "--direct":
		return runRun(args, stdout, stderr)
	case "--no-policy":
		if len(args) == 1 {
			return launch("aggy", target{NoPolicy: true}, stderr)
		}
		return runRun(args, stdout, stderr)
	case "new":
		return runNew(args[1:], stdout, stderr)
	case "resume":
		return runResume(args[1:], stdout, stderr)
	case "sessions":
		return runSessions(args[1:], stdout, stderr)
	case "session":
		return runSession(args[1:], stdout, stderr)
	case "setup":
		return runSetup(args[1:], stdout, stderr)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr)
	case "log":
		return runLog(args[1:], stdout, stderr)
	case "diag":
		return runDiag(args[1:], stdout, stderr)
	case "stats":
		return runStats(args[1:], stdout, stderr)
	case "dev":
		return runDev(args[1:], stdout, stderr)
	case "unwire":
		return runUnwire(args[1:], stdout, stderr)
	case "skill":
		return runSkill(args[1:], stdout, stderr)
	case "hooks":
		return runHooks(args[1:], stdout, stderr)
	case "memory":
		return runMemory(args[1:], stdout, stderr)
	case "policy":
		return runPolicy(args[1:], stdout, stderr)
	case "mcp":
		return runMCP(args[1:], stdout, stderr)
	case "attribution":
		return runAttribution(args[1:], stdout, stderr)
	case "check":
		return runCheck(args[1:], stdout, stderr)
	case "gc":
		return runGC(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "aggy: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
