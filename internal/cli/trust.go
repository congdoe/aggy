package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const trustUsage = `Usage: aggy trust --opencode <dir>

Lets the repo holding <dir> open the full OpenCode screen under aggy, with
OpenCode's own model command, and reads with no prompt. Without it, a repo
gets opencode mini and the whitelist. [opencode] auto = true in aggy's
config then applies there too. aggy unwire undoes it.

It asks once, at a terminal. It records the repo root, so a subfolder is
treated the same. A repo cannot trust itself: this is kept in aggy's own
state folder. Trusting a repo here does not change how Codex or Grok load it.

  --opencode   trust it for OpenCode (the only kind of trust so far)
  --dry-run    show the change and make none
`

// trustWarning says what trusting a repo for OpenCode means. It is the
// design's own words (docs/design.md, section 15).
const trustWarning = `Trust %s for OpenCode?

- The repo's screen plugins (.opencode/plugins/*/tui.js) WILL run, as you, before
  any guard, each time the full screen opens. No setting stops them.
- Reads in this repo run with no prompt, so a link in the repo can reach a file
  outside it; aggy's guard refuses what it can see, and only that. grep's search
  text is not covered: a deny by file name applies to the path it searches.
- aggy gives guardrails against mistakes, not protection from a hostile repo or a
  tricked model. A model's shell runs as you with no OS sandbox.

The repo's own OpenCode config stays off.
`

// trustDone reports a trust. Trusts share one file, so a later one has to be
// undone before this one: aggy unwire refuses, and says which is in its way.
const trustDone = "Trusted %s for OpenCode.\nDone (run %s). aggy unwire --run %s undoes it, once any trust made after it is undone.\n"

// runTrust is aggy trust.
func runTrust(args []string, stdout, stderr io.Writer) int {
	ctx := context.Background()
	fl := flag.NewFlagSet("trust", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	forOpenCode := fl.Bool("opencode", false, "")
	dryRun := fl.Bool("dry-run", false, "")
	usage := func() int {
		refuse(ctx, stderr, "trust", diag.EvTrustRefused, diag.CodeUsage, "usage: aggy trust --opencode <dir>", diag.Rec{})
		return 2
	}
	if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Fprint(stdout, trustUsage)
		return 0
	}
	if err := fl.Parse(args); err != nil {
		return usage()
	}
	rest := fl.Args()
	// flag stops at the first argument, and the design writes the flag first.
	for len(rest) > 1 && strings.HasPrefix(rest[1], "--") {
		switch rest[1] {
		case "--opencode":
			*forOpenCode = true
		case "--dry-run":
			*dryRun = true
		default:
			return usage()
		}
		rest = append(rest[:1:1], rest[2:]...)
	}
	if !*forOpenCode || len(rest) != 1 {
		return usage()
	}
	fail := func(code diag.Code, err error) int {
		refuse(ctx, stderr, "trust", diag.EvTrustRefused, code, err.Error(), diag.Rec{})
		return 1
	}
	fi, err := safefs.Stat(rest[0])
	switch {
	case err != nil:
		return fail(diag.CodeTrustBadDir, err)
	case !fi.IsDir():
		return fail(diag.CodeTrustBadDir, fmt.Errorf("%s is not a folder", rest[0]))
	}
	dir, err := safefs.RealPath(rest[0])
	if err != nil {
		return fail(diag.CodeTrustBadDir, err)
	}
	root, _ := repoRoot(dir)
	key, err := safefs.CanonicalPath(root)
	if err != nil {
		return fail(diag.CodeTrustBadDir, err)
	}
	path, err := openCodeTrustFile()
	if err != nil {
		return fail(diag.CodeStore, err)
	}
	p, already, err := trustPatch(path, key)
	if err != nil {
		return fail(diag.CodeTrustBadFile, err)
	}
	if already {
		fmt.Fprintf(stdout, "%s is already trusted for OpenCode.\n", termsafe.Field(root))
		return 0
	}
	ps := []patch.Patch{p}
	if *dryRun {
		if _, err := manifest.Preview(stdout, ps); err != nil {
			return 1
		}
		return 0
	}
	// A model's tool shell has no terminal on stdin, so it cannot trust its
	// own repo this way. This guards against mistakes, not a model: a
	// same-user process can give itself a pseudo-terminal.
	if !isTerminal(os.Stdin) {
		return fail(diag.CodeNoTerminal, errors.New("trusting a repo asks you at a terminal, and stdin is not one"))
	}
	fmt.Fprintf(stdout, trustWarning, termsafe.Field(root))
	fmt.Fprint(stdout, "Trust it? [y/N] ")
	line, _ := bufio.NewReader(io.LimitReader(os.Stdin, maxAnswer)).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
		fmt.Fprintln(stdout, "Not trusted.")
		return 1
	}
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return fail(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		return fail(diag.CodeStore, err)
	}
	runID := manifest.NewRunID()
	if _, err := m.Run(ctx, manifest.Write, runID, ps, io.Discard); err != nil {
		return fail(diag.CodeTrustBadFile, err)
	}
	fmt.Fprintf(stdout, trustDone, termsafe.Field(root), runID, runID)
	return 0
}

// trustPatch adds key to the trust file at path, or reports it is there.
func trustPatch(path, key string) (patch.Patch, bool, error) {
	h := patch.NoFile
	b, err := safefs.ReadConfig(path, config.MaxFileSize)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// A first trust: nothing to keep.
	case err != nil:
		return patch.Patch{}, false, err
	default:
		h = patch.Hash(b)
		var t openCodeTrust
		if err := json.Unmarshal(b, &t); err != nil {
			return patch.Patch{}, false, fmt.Errorf("%s is not valid: %w", path, err)
		}
		if slices.Contains(t.Repos, key) {
			return patch.Patch{}, true, nil
		}
	}
	return patch.Patch{File: path, ExpectedHash: h, Mode: 0o600, Transform: func(src []byte) ([]byte, error) {
		var t openCodeTrust
		if len(src) > 0 {
			if err := json.Unmarshal(src, &t); err != nil {
				return nil, err
			}
		}
		if !slices.Contains(t.Repos, key) {
			t.Repos = append(t.Repos, key)
		}
		slices.Sort(t.Repos)
		out, err := json.MarshalIndent(t, "", "  ")
		if err != nil {
			return nil, err
		}
		return append(out, '\n'), nil
	}}, false, nil
}
