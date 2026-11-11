package cli

import (
	"fmt"
	"io"
)

const (
	runHelpText = `Usage: aggy run [--direct] [--no-policy] [--tool NAME]... <claude|codex|grok|opencode>

Starts a new session of that CLI here, under aggy. For the CLI's own flags,
use aggy claude, aggy codex, aggy grok, or aggy opencode.

  --direct      On your terminal, without aggy's pass-through (no switch key)
  --no-policy   Without aggy's safety policy
  --tool NAME   Turn a tool on for this session only
`
	newUsage = `Usage: aggy new [--no-policy] [claude|codex|grok|opencode]

Starts another session in this directory. The old ones stay listed. Without
a vendor it uses default_vendor, else the first one connected.
`
	resumeUsage = `Usage: aggy resume [--no-policy] [--brief [--show]] [ID]

Reopens the latest session here, or starts one, or the session with this id
(a unique prefix), wherever it is. An id from aggy session reopens that child:
an interactive one, or a borrow's once it has ended.

  --brief       Write the child's handoff brief and tell the child to read it
  --show        With --brief: print the brief and start nothing
  --no-policy   Without aggy's safety policy
`
	continueUsage = `Usage: aggy --continue [--no-policy] [the CLI's own flags]

Reopens the session last active in this directory that can still be reopened,
or refuses when there is none. Also -c. aggy <vendor> --continue does so for
that vendor. Flags after it go to that session's CLI, sorted by its vendor's
rules (aggy help claude).
`
	sessionsUsage = `Usage: aggy sessions

Lists every session: its id, last active time, last vendor, directory, and
title. aggy resume <id> reopens one.
`
	sessionUsage = `Usage: aggy session

Lists the children of the open session (or the latest in this directory):
vendor, kind, title, model, last active, and status. Borrows are children too.
aggy resume <child id> reopens one, except a research or news child: aggy inbox
has its report.
`
	versionUsage = `Usage: aggy version

Prints the aggy version.
`
	helpUsage = `Usage: aggy help [COMMAND]

Shows aggy's commands, or one command's help. aggy COMMAND --help is the same.
`
	hookToolUsage = `Usage: aggy hook-tool ...

For the hooks aggy wires into a session for a managed tool, not for people.
`
)

// commandHelp is each command's help, by every name Run takes it under.
var commandHelp = map[string]string{
	"borrow": borrowUsage, "image": imageUsage, "video": videoUsage, "inbox": inboxUsage,
	"accept": acceptUsage, "reject": acceptUsage, "brief": briefUsage, "hook": hookUsage,
	"hook-tool": hookToolUsage, "trust": trustUsage, "switch": switchUsage, "run": runHelpText, "--direct": runHelpText,
	"new": newUsage, "resume": resumeUsage, "--resume": resumeUsage, "-r": resumeUsage,
	"--continue": continueUsage, "-c": continueUsage, "--no-policy": runHelpText,
	"claude": vendorUsage, "codex": vendorUsage, "grok": vendorUsage, "opencode": vendorUsage, "hermes": vendorUsage, "antigravity": vendorUsage,
	"sessions": sessionsUsage, "session": sessionUsage, "setup": setupUsage, "doctor": doctorUsage,
	"log": logUsage, "diag": diagUsage, "stats": statsUsage, "usage": usageUsage, "dev": devUsage,
	"unwire": unwireUsage, "skill": skillUsage, "tool": toolUsage, "hooks": hooksUsage,
	"key": keyUsage, "memory": memoryUsage, "policy": policyUsage, "permissions": permissionsUsage, "config": configUsage, "mcp": mcpUsage, "attribution": attributionUsage,
	"check": checkUsage, "gc": gcUsage, "version": versionUsage, "--version": versionUsage, "-v": versionUsage,
	"help": helpUsage,
}

// wantsHelp reports whether args ask for help: --help or -h anywhere before
// a "--", after which arguments belong to something else (aggy dev capture).
func wantsHelp(args []string) bool {
	for _, a := range args {
		switch a {
		case "--":
			return false
		case "--help", "-h":
			return true
		}
	}
	return false
}

// runHelp is aggy help [COMMAND].
func runHelp(args []string, stdout, stderr io.Writer) int {
	switch len(args) {
	case 0:
		fmt.Fprint(stdout, usage)
		return 0
	case 1:
		if u, ok := commandHelp[args[0]]; ok {
			fmt.Fprint(stdout, u)
			return 0
		}
		return cmdUsage(stderr, "help", fmt.Sprintf("unknown command %q", args[0]), usage)
	}
	return cmdUsage(stderr, "help", "name one command", helpUsage)
}
