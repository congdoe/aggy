package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const attributionUsage = `Usage: aggy attribution wire [--dry-run]

Installs a git commit-msg hook in the repo you are in that adds
Aggy-Vendor and Aggy-Model trailers to commits made under a CLI aggy
runs (aggy sets AGGY_VENDOR and AGGY_MODEL for each child). Your own
commits, made outside aggy, get nothing. A review borrow routes on
aggy's own record of which vendor made an edit, and reads the
Aggy-Vendor trailers only as a hint of more vendors to pass over:
anyone can write a trailer.

The hook goes in the repo's common git dir, so it covers every worktree
of the repo, edit borrows' included. It is fixed text: nothing from the
repo is written into it, and it adds only values made of letters,
digits, and . _ : -. aggy never replaces a hook: when a commit-msg hook
exists or core.hooksPath is set, wire refuses (chaining hooks is not
supported yet). aggy unwire removes it.

wire reads git config with aggy's scrubbed environment, so a
core.hooksPath set only through XDG_CONFIG_HOME, GIT_CONFIG_GLOBAL, or
GIT_CONFIG_COUNT/KEY/VALUE is not seen: the hook is then installed but
git never runs it.

  --dry-run   show the change and make none
`

func runAttribution(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "wire" {
		if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
			fmt.Fprint(stdout, attributionUsage)
			return 0
		}
		fmt.Fprint(stderr, attributionUsage)
		return 2
	}
	fl := flag.NewFlagSet("attribution wire", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	dryRun := fl.Bool("dry-run", false, "")
	if err := fl.Parse(args[1:]); err != nil || fl.NArg() > 0 {
		fmt.Fprintf(stderr, "aggy attribution wire: bad arguments\n\n%s", attributionUsage)
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "aggy attribution wire: %s\n", termsafe.Line(err.Error()))
		return 1
	}
	env := defaultBorrowEnv()
	ctx := context.Background()
	cwd, err := env.cwd()
	if err != nil {
		return fail(err)
	}
	path, err := env.attributionHookPath(ctx, cwd)
	if err != nil {
		return fail(err)
	}
	p, err := parity.AttributionHookPatch(path)
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

// attributionHookPath returns where the attribution hook goes for the
// repo holding cwd: <git common dir>/hooks/commit-msg. It refuses a dir in
// no work tree, and a repo whose config sets core.hooksPath at any scope,
// since git would then never run a hook there (or would run another
// one). git runs with gitSafe, so no program from the repo's config runs.
// It runs with the scrubbed environment, so a core.hooksPath set only
// through XDG_CONFIG_HOME, GIT_CONFIG_GLOBAL, or GIT_CONFIG_COUNT is not
// seen; the usage text says so.
func (e borrowEnv) attributionHookPath(ctx context.Context, cwd string) (string, error) {
	root, ok := repoRoot(cwd)
	if !ok {
		return "", fmt.Errorf("%s is in no git work tree", cwd)
	}
	out, err := e.gitOutput(ctx, root, "config", "--show-scope", "--get-all", "core.hooksPath")
	if err != nil {
		return "", err
	}
	if out.ExitCode != 0 && out.ExitCode != 1 {
		return "", fmt.Errorf("git config: %s", strings.TrimSpace(string(out.Stderr)))
	}
	for _, l := range strings.Split(string(out.Stdout), "\n") {
		// gitSafe's own -c core.hooksPath shows as the command scope.
		if scope, _, _ := strings.Cut(l, "\t"); l != "" && scope != "command" {
			return "", errors.New("core.hooksPath is set in your git config, so aggy will not install a hook in the repo's hooks folder (chaining hooks is not supported yet)")
		}
	}
	dir, _, err := e.git(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	dir = strings.TrimSpace(dir)
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || strings.ContainsAny(dir, "\n\x00") {
		return "", fmt.Errorf("git gave an unexpected git dir %q", dir)
	}
	return filepath.Join(dir, "hooks", parity.AttributionHookName), nil
}
