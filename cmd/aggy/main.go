// Command aggy runs the official Claude Code, Codex, and Grok Build CLIs
// with shared sessions, handoff briefs, and borrows by task kind.
package main

import (
	"os"

	"github.com/congdoe/aggy/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
