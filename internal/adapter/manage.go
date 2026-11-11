package adapter

// Management is a vendor CLI's management subcommands, which aggy runs as
// typed, with nothing added (T6963): the ones that run no agent. They live here,
// not in each vendor's package, so the guard hook (internal/hook) can read them
// without importing an adapter.
type Management struct {
	// Commands are the subcommands, as the first word after the vendor.
	Commands []string
	// Refused are second words of one of Commands that run an agent after all,
	// as claude mcp serve.
	Refused map[string][]string
}

// Manage is each vendor's Management, after the PR #124 review: anything that
// starts, attaches to, or respawns an agent, or serves the CLI's own tools, is
// not here, and stays refused as a session's first word with a reason that
// names the vendor's CLI.
//
// Claude, from the CLI reference's commands table
// (code.claude.com/docs/en/cli-reference, read 2026-11-09): install and update
// the binary, sign in (auth, setup-token), configure MCP servers and plugins,
// print diagnostics (doctor, auto-mode), and print, stop, or remove background
// sessions (logs, stop, kill, rm, purge). Not here: agents dispatches
// background sessions and takes --permission-mode and --settings, attach and
// respawn reattach to or restart one, daemon run hosts them, and gateway,
// import, rc and remote-control, self-hosted-runner, and ultrareview start or
// move work aggy does not record. mcp serve serves Claude Code's own tools to an
// MCP client (Claude Code's MCP docs, "Use Claude Code as an MCP server"; not
// in the reference's table, and not re-read for this change).
//
// Codex, from the Subcommand enum of codex-rs/cli/src/main.rs at 6aaac69
// (read 2026-11-09): sign-in, MCP servers, plugins, shell completion,
// diagnostics, and updates. Not here: apply (a) writes a cloud task's diff,
// which aggy did not review, into this tree; features persists a feature flag,
// hooks and sandbox ones included, past aggy's launch gate; and the session and
// server commands (resume, fork, exec, review, app-server, exec-server, and the
// rest) run an agent. A server mode of codex mcp is unverified at 6aaac69 and
// refused on the safe side.
//
// Grok, from the Command enum in xai-grok-pager/src/app/cli.rs at grok-build
// 2bdd1d6a (read 2026-11-09): sign-in, MCP servers, plugins, the model list,
// usage, diagnostics, completions, updates, and the version. Not here: setup and
// memory write the config and memory aggy keeps in parity, and agent, leader,
// wrap, worktree, workspace, sessions, share, export, trace, inspect, and
// dashboard run or reach a session. A server mode of grok mcp is unverified at
// 2bdd1d6a and refused on the safe side.
//
// OpenCode, the subcommands recorded live at 2.2.5 (docs/verification.md: auth
// list, models, mcp list). debug is not here: some of its subcommands may run
// an agent or a tool, which aggy has not checked at 2.2.5; nor upgrade, which is
// not recorded (unverified).
var Manage = map[Vendor]Management{
	Claude: {
		Commands: []string{"auth", "auto-mode", "doctor", "install", "kill", "logs", "mcp", "plugin", "plugins", "purge", "rm",
			"setup-token", "stop", "update"},
		Refused: map[string][]string{"mcp": {"serve"}},
	},
	Codex: {
		Commands: []string{"completion", "doctor", "login", "logout", "mcp", "plugin", "update"},
		Refused:  map[string][]string{"mcp": {"serve", "server"}},
	},
	Grok: {
		Commands: []string{"completions", "doctor", "login", "logout", "mcp", "models", "plugin", "update", "usage", "v", "version"},
		Refused:  map[string][]string{"mcp": {"serve", "server"}},
	},
	OpenCode: {
		Commands: []string{"auth", "mcp", "models"},
	},
	// Hermes Agent 0.23.1: sign-in and the model picker, in the person's own
	// default profile (hermes --help lists both, live; docs/verification.md).
	// Not here: config, profile, sessions, hooks, and gateway change or reach
	// what aggy keeps (its profile, the hook consent, the daemon).
	Hermes: {
		Commands: []string{"auth", "model"},
	},
}
