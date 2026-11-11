package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
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
itself, gets past it. It never loosens a setting of yours or a repo's,
and a repo cannot edit it (but see Codex below).

On screen the CLI asks you before each of these, where it can: Claude
Code and OpenCode's shell rules ask, Grok Build has no way to be told to
ask, so there they stay denials, and OpenCode's file reads stay refused
by aggy's guard. A borrow child is always refused. On Claude Code and
OpenCode, aggy's own hook asks for every match but a command you
pre-approved exactly (aggy permissions add allow), which runs; Codex
and Grok Build are not changed by a pre-approval. aggy permissions
policy moves a group to deny (aggy permissions show says what each CLI
gets).

Claude Code and Grok Build get it on the command line of every session
aggy starts (claude --settings, grok --deny), so nothing is stored.
Codex has no such flag: wire writes ~/.codex/rules/aggy.rules
($CODEX_HOME/rules) through the manifest, as rules Codex asks about
(refused under approval never, as codex exec runs), never over a file of
yours but aggy's own older one, and aggy unwire removes it. The file is
global, so a per-repo setting does not reach Codex.
Codex's sandbox and approval settings are
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
		return cmdUsage(stderr, "policy", "name a subcommand", policyUsage)
	}
	fl := flag.NewFlagSet("policy wire", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	dryRun := fl.Bool("dry-run", false, "")
	if err := fl.Parse(args[1:]); err != nil || fl.NArg() > 0 {
		return cmdUsage(stderr, "policy wire", "bad arguments", policyUsage)
	}
	fail := func(err error) int {
		return cmdErrDry(stderr, "policy wire", diag.CodeWireFailed, err, *dryRun)
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
			return fail(err)
		}
		return 0
	}
	if p.ExpectedHash == patch.Hash([]byte(parity.CodexRules)) {
		fmt.Fprintln(stdout, "Nothing to change.")
		return 0
	}
	if err := applyCodexRules(context.Background(), path, ps, stdout); err != nil {
		return fail(err)
	}
	return 0
}

// applyCodexRules writes the Codex rules file through the manifest, as aggy
// policy wire does, and says so on out. A launch that asked in place
// (T6979) uses it too.
func applyCodexRules(ctx context.Context, path string, ps []patch.Patch, out io.Writer) error {
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		return err
	}
	runID := manifest.NewRunID()
	if _, err := m.Run(ctx, manifest.Write, runID, ps, io.Discard); err != nil {
		return err
	}
	fmt.Fprintf(out, "changed %s\nDone (run %s). aggy unwire --run %s undoes it.\n", termsafe.Field(path), runID, runID)
	return nil
}

// switchFix is the supervisor's SwitchFix (T6979): a switch to Codex that the
// missing rules file refused is offered aggy policy wire on aggy's page.
func switchFix(v adapter.Vendor, err error) (lines, confirm []string, ok bool) {
	if v != adapter.Codex || err == nil || !strings.Contains(err.Error(), whyRulesMissed) {
		return nil, nil, false
	}
	path, perr := parity.CodexRulesPath()
	if perr != nil {
		return nil, nil, false
	}
	return []string{
		"Codex has no aggy rules file, so aggy's safety policy cannot reach it.",
		"",
		"y writes " + path + " now, as aggy policy wire does, and the switch goes on;",
		"aggy unwire undoes it. Esc leaves things as they are.",
	}, []string{"policy", "wire"}, true
}

// wireCodexRulesInPlace offers, at a launch of Codex whose rules file is not
// written, to write it now (T6979), and reports whether it was written.
func wireCodexRulesInPlace(ctx context.Context, getenv func(string) string, in io.Reader, out io.Writer) (bool, error) {
	path, err := parity.CodexRulesPath()
	if err != nil {
		return false, err
	}
	p, err := parity.CodexRulesPatch(path)
	if err != nil {
		return false, err
	}
	yes, asked := askInPlace(getenv, in, out, inPlace{
		change: "Codex has no aggy rules file, so aggy's safety policy cannot reach it. aggy can write " + path + " now, as aggy policy wire does.",
		undo:   "aggy unwire undoes it.",
		ask:    "Write it and start Codex?",
	})
	if !asked || !yes {
		return false, nil
	}
	return true, applyCodexRules(ctx, path, []patch.Patch{p}, out)
}
