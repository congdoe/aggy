package claude

// Claude Code's own sandbox (T6980, docs/design.md section 10, "The vendors'
// own sandboxes"). Every fact here is from Claude Code's docs, read
// 2026-11-11. A maintainer's live run on 2.1.322 (2026-11-11,
// docs/verification.md, "The vendors' own sandboxes") confirmed that
// enabled and autoAllowBashIfSandboxed are read from --settings, that
// auto-allow drops the prompt for an in-folder command and false brings it
// back, that a write outside the project is blocked and its unsandboxed
// retry asks, and that a PreToolUse hook's ask still prompts under
// auto-allow. failIfUnavailable, the platform facts, and nesting are not
// live-checked (the checks owed are listed there):
//
//   - code.claude.com/docs/en/sandboxing: "The sandbox is off by default";
//     set sandbox.enabled to true in a settings file. It "covers shell
//     commands only": Bash, PowerShell, and Monitor commands and the
//     processes they start. The file and web tools (Read, Edit, Write,
//     WebFetch, WebSearch), command hooks, and MCP servers run outside it.
//     Writes: the working directory, a per-user temp folder, and folders
//     added. Reads: most of the machine. Network: none direct; a proxy
//     checks each host against allowed domains, which start empty.
//   - Same page: Claude may retry a blocked command unsandboxed with the
//     dangerouslyDisableSandbox parameter; in manual and acceptEdits modes
//     that asks ("Bash command (unsandboxed)"), and sandbox.
//     allowUnsandboxedCommands false forbids it. aggy leaves that as Claude
//     sets it: the retry asks the person, and aggy's decision hook judges
//     every Bash call, sandboxed or not.
//   - Same page, "Sandbox modes": in auto-allow mode (sandbox.
//     autoAllowBashIfSandboxed, default true) Claude "approves a command
//     automatically, with no prompt, when the command runs inside the
//     sandbox"; in regular permissions mode "All Bash commands go through
//     the regular permission flow, even when sandboxed". Reads still reach
//     most of the machine, so auto-allow would drop the person's prompts for
//     a cat of ~/.aws/credentials or an rm -rf inside the folder (PR #135
//     review). aggy sets it false: the sandbox only adds confinement, and
//     prompting stays as it was without it.
//     TODO(T6954): auto-allow could become a separate, plainly worded choice.
//   - Same page: it runs on macOS (Seatbelt, built in), Linux and WSL2
//     (bubblewrap and socat must be installed); on native Windows commands
//     run unsandboxed. "By default, when the platform doesn't support the
//     sandbox or the dependency check at startup fails, Claude Code runs
//     commands without sandboxing"; sandbox.failIfUnavailable true makes it
//     exit at startup instead. aggy sets it, so a launch line that says the
//     sandbox is on is never a session that runs without it.
//   - code.claude.com/docs/en/settings, "Change a setting for one session":
//     --settings "can set any key your user settings file can set", and the
//     settings index gives sandbox.enabled, failIfUnavailable,
//     allowUnsandboxedCommands, and autoAllowBashIfSandboxed the scope "Any
//     file". --settings sits above the user and project files.

// SandboxLayer is the "sandbox" value aggy's inline --settings layer holds
// when the folder's trust list ticks Claude's sandbox: on, Claude exits at
// startup rather than run its shell unsandboxed, and every Bash command still
// goes through Claude's permission flow (no auto-allow).
func SandboxLayer() map[string]any {
	return map[string]any{"enabled": true, "failIfUnavailable": true, "autoAllowBashIfSandboxed": false}
}
