package claude

// Claude Code's own sandbox (T6980, docs/design.md section 10, "The vendors'
// own sandboxes"). Every fact here is from Claude Code's docs, read
// 2026-11-11, and none is live-checked at 2.1.322, the version aggy's other
// Claude checks ran on (docs/verification.md lists the checks owed):
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
//   - Same page: in its default auto-allow mode (sandbox.
//     autoAllowBashIfSandboxed, default true), a sandboxed command runs with
//     no permission prompt, but deny rules and content-scoped ask rules
//     (Bash(git push *)) still apply; a bare Bash ask rule does not. aggy
//     leaves that default too.
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
// when the folder's trust list ticks Claude's sandbox: on, and Claude exits
// at startup rather than run its shell unsandboxed.
func SandboxLayer() map[string]any {
	return map[string]any{"enabled": true, "failIfUnavailable": true}
}
