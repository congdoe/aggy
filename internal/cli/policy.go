package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const policyUsage = `Usage: aggy policy wire [--dry-run]

aggy's safety policy: never read .env or key files, never push to main,
no destructive commands (rm -rf, git reset --hard, git clean, force
push). It is a guardrail against an agent's mistakes, not a sandbox:
each CLI matches its rules against the command text or file path it
sees, so a command written another way, or a script that opens a file
itself, gets past it. It only adds denials, never loosens a setting of
yours or a repo's, and a repo cannot edit it (but see Codex below).

Claude Code and Grok Build get it on the command line of every session
aggy starts (claude --settings, grok --deny), so nothing is stored.
Codex has no such flag: wire writes ~/.codex/rules/aggy.rules
($CODEX_HOME/rules) through the manifest, never over a file of yours,
and aggy unwire removes it. Codex's sandbox and approval settings are
left as you set them, and its rules cannot stop a file read, so .env is
not covered there. Codex drops every rules file, aggy's included, when
any rules file it loads fails to parse, a trusted repo's .codex/rules
among them; aggy doctor lists those files.

  --dry-run   show the change and make none
`

func runPolicy(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "wire" {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			fmt.Fprint(stdout, policyUsage)
			return 0
		}
		fmt.Fprint(stderr, policyUsage)
		return 2
	}
	fl := flag.NewFlagSet("policy wire", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	dryRun := fl.Bool("dry-run", false, "")
	if err := fl.Parse(args[1:]); err != nil || fl.NArg() > 0 {
		fmt.Fprintf(stderr, "aggy policy wire: bad arguments\n\n%s", policyUsage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "aggy policy wire: %s\n", termsafe.Line(err.Error()))
		return 1
	}
	path, err := parity.CodexRulesPath()
	if err != nil {
		return fail(err)
	}
	p, err := parity.CodexRulesPatch(path)
	if err != nil {
		return fail(err)
	}
	ps := []patch.Patch{p}
	if *dryRun {
		if _, err := manifest.Preview(stdout, ps); err != nil {
			return 1
		}
		return 0
	}
	if p.ExpectedHash != patch.NoFile {
		fmt.Fprintln(stdout, "Nothing to change.")
		return 0
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		return fail(err)
	}
	runID := manifest.NewRunID()
	if _, err := m.Run(ctx, manifest.Write, runID, ps, io.Discard); err != nil {
		return fail(err)
	}
	fmt.Fprintf(stdout, "changed %s\nDone (run %s). aggy unwire --run %s undoes it.\n", termsafe.Field(path), runID, runID)
	return 0
}
