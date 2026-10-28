package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const mcpUsage = `Usage: aggy mcp wire [--vendor codex|grok] [--dry-run]

Copies the MCP servers of Claude Code's user scope (~/.claude.json) into
Codex (~/.codex/config.toml, or $CODEX_HOME) and Grok Build
(~/.grok/config.toml, or $GROK_HOME), through the manifest, so aggy
unwire takes them out again. A server the CLI already has by name is
left as it is.

Only servers that hold no secret are copied. One that sets environment
variables or headers, has OAuth client settings, uses ${VAR} expansion,
or has an argument or URL that may carry a token is listed for you to
add yourself. claude.ai connectors live in your Claude account, not in
that file, and cannot be copied. Local-scope servers (one project only)
and a repo's .mcp.json are never copied.

A remote server that needs a sign-in: run codex mcp login <name> for
Codex, which keeps the token in its own store; Grok asks on first use.

  --vendor V  wire only codex or grok (default: both)
  --dry-run   show the changes and make none
`

func runMCP(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "wire" {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			fmt.Fprint(stdout, mcpUsage)
			return 0
		}
		fmt.Fprint(stderr, mcpUsage)
		return 2
	}
	fl := flag.NewFlagSet("mcp wire", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	dryRun := fl.Bool("dry-run", false, "")
	only := fl.String("vendor", "", "")
	if err := fl.Parse(args[1:]); err != nil || fl.NArg() > 0 {
		fmt.Fprintf(stderr, "aggy mcp wire: bad arguments\n\n%s", mcpUsage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "aggy mcp wire: %s\n", termsafe.Line(err.Error()))
		return 1
	}
	vendors := []adapter.Vendor{adapter.Codex, adapter.Grok}
	switch *only {
	case "":
	case "codex", "grok":
		vendors = []adapter.Vendor{adapter.Vendor(*only)}
	default:
		fmt.Fprintf(stderr, "aggy mcp wire: --vendor must be codex or grok\n\n%s", mcpUsage)
		return 2
	}
	src, err := parity.ClaudeJSONPath(os.Getenv)
	if err != nil {
		return fail(err)
	}
	servers, skips, err := parity.ReadClaudeMCP(src)
	if err != nil {
		return fail(err)
	}
	for _, s := range skips {
		fmt.Fprintf(stdout, "not copied: %s: %s; add it yourself if you want it\n", termsafe.Line(s.Name), s.Why)
	}
	var ps []patch.Patch
	login := false
	for _, v := range vendors {
		path, err := parity.MCPConfigPath(v, os.Getenv)
		if err != nil {
			return fail(err)
		}
		w, err := parity.PlanMCPWire(v, path, servers)
		if err != nil {
			return fail(fmt.Errorf("%s: %w", v, err))
		}
		for _, n := range w.Have {
			fmt.Fprintf(stdout, "%s already has %s; left as it is\n", v, termsafe.Line(n))
		}
		for _, s := range w.Skip {
			fmt.Fprintf(stdout, "not copied to %s: %s: %s\n", v, termsafe.Line(s.Name), s.Why)
		}
		for _, s := range w.Add {
			fmt.Fprintf(stdout, "%s gets %s\n", v, s.Name)
			login = login || (v == adapter.Codex && s.Type != "stdio")
		}
		if len(w.Add) > 0 {
			ps = append(ps, w.Patch)
		}
	}
	if len(ps) == 0 {
		fmt.Fprintln(stdout, "Nothing to change.")
		return 0
	}
	if *dryRun {
		if _, err := manifest.Preview(stdout, ps); err != nil {
			return 1
		}
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
	for _, p := range ps {
		fmt.Fprintf(stdout, "changed %s\n", termsafe.Field(p.File))
	}
	if login {
		fmt.Fprintln(stdout, "A remote server that needs a sign-in in Codex: run codex mcp login <name>.")
	}
	fmt.Fprintf(stdout, "Done (run %s). aggy unwire --run %s undoes it.\n", runID, runID)
	return 0
}
