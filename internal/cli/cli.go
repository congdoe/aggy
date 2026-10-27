// Package cli parses aggy's command line and dispatches to commands.
package cli

import (
	"fmt"
	"io"
)

// Version is set at build time:
//
//	go build -ldflags "-X github.com/congdoe/aggy/internal/cli.Version=v0.1.0"
var Version = "dev"

const usage = `aggy runs the official Claude Code, Codex, and Grok Build CLIs.

Usage:
  aggy <command>

Commands:
  log       Show harnesses, models, switches, borrows, and files changed
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
		fmt.Fprintf(stdout, "aggy %s\n", Version)
		return 0
	case "log":
		return runLog(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "aggy: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
