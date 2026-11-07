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
	"path/filepath"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const trustUsage = `Usage: aggy trust <dir>
       aggy trust --opencode <dir>

aggy trust <dir> says you trust the repo holding <dir>: your allow rules that
run a repo's own code (make, npm test, ./scripts/x) apply there, and so do your
approvals from Codex, and Codex loads it as trusted. Claude and Grok keep their
own trust, which aggy does not handle yet: each still asks you itself.

aggy trust --opencode <dir> is separate. It lets the repo open the full OpenCode
screen under aggy, with OpenCode's own model command, and reads with no prompt.
Without it, a repo gets opencode mini and the whitelist. [opencode] auto = true
in aggy's config then applies there too. It does not change how Codex or Grok
load the repo.

Each asks once, at a terminal, and never from a session's own tool shell. Each
records the repo root, so a subfolder is treated the same. A repo cannot trust
itself: this is kept in aggy's own state folder. aggy unwire undoes it.

  --opencode   trust it for OpenCode
  --dry-run    show the change and make none
`

// repoTrustWarning says what aggy trust <dir> means. The Claude and Grok lines
// stay until T6895's live check says where they keep a folder's trust.
// TODO(T6895): record the folder for Claude and Grok too, through the manifest.
// Do not read ~/.claude.json for it before then.
const repoTrustWarning = `Trust %s?

- aggy: your allow rules that run a repo's own code (make, npm test, ./scripts/x),
  and your approvals from Codex, now apply here.
- Codex: %s
- Claude and Grok: not handled yet. aggy does not know where they keep a folder's
  trust, so each still asks you itself when it first opens this folder.
- Once Grok trusts this folder, it loads the repo's committed .claude/settings.json
  itself, hooks and allow rules included. aggy cannot filter what Grok reads.
- aggy gives guardrails against mistakes, not protection from a hostile repo or a
  tricked model. A model's shell runs as you with no OS sandbox.

`

const (
	codexTrustWill = "%s gains trust_level = \"trusted\" for this folder, so Codex\n  loads the repo's own config, hooks, and rules. (aggy takes this to be Codex's\n  key; it has not been checked against a live Codex.)"
	codexTrustHas  = "%s already trusts this folder."
	codexTrustNone = "no Codex config folder here, so nothing changes for it."
)

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
		refuse(ctx, stderr, "trust", diag.EvTrustRefused, diag.CodeUsage, "usage: aggy trust [--opencode] <dir>", diag.Rec{})
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
	if len(rest) != 1 {
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
	if !*forOpenCode {
		return trustFolder(ctx, dir, *dryRun, fail, stdout)
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
	if err := personGate(os.Getenv); err != nil {
		return fail(diag.CodeNoTerminal, err)
	}
	fmt.Fprintf(stdout, trustWarning, termsafe.Field(root))
	if !confirmed(stdout, "Trust it?") {
		fmt.Fprintln(stdout, "Not trusted.")
		return 1
	}
	runID, err := applyPatches(ctx, ps)
	if err != nil {
		return fail(diag.CodeTrustBadFile, err)
	}
	fmt.Fprintf(stdout, trustDone, termsafe.Field(root), runID, runID)
	return 0
}

// trustFolder is aggy trust <dir>: aggy's own record, and Codex's key, as one run
// of the manifest. A Codex config it cannot change is said and left alone: the
// person asked to trust the repo, and aggy's record does not depend on it.
func trustFolder(ctx context.Context, dir string, dryRun bool, fail func(diag.Code, error) int, stdout io.Writer) int {
	repo, err := toolRepo(dir, false)
	if err != nil {
		return fail(diag.CodeTrustBadDir, err)
	}
	main, key := repo.main, repo.key
	if main == "" {
		if key, err = safefs.CanonicalPath(dir); err != nil {
			return fail(diag.CodeTrustBadDir, err)
		}
		main = dir
	}
	path, err := trustFile()
	if err != nil {
		return fail(diag.CodeStore, err)
	}
	p, already, err := trustPatch(path, key)
	if err != nil {
		return fail(diag.CodeTrustBadFile, err)
	}
	var ps []patch.Patch
	if !already {
		ps = append(ps, p)
	}
	codexLine, codexPatch := codexTrust(main)
	if codexPatch != nil {
		ps = append(ps, *codexPatch)
	}
	if len(ps) == 0 {
		fmt.Fprintf(stdout, "%s is already trusted.\n", termsafe.Field(main))
		return 0
	}
	if dryRun {
		fmt.Fprintf(stdout, "Codex: %s\n", termsafe.Line(codexLine))
		if _, err := manifest.Preview(stdout, ps); err != nil {
			return 1
		}
		return 0
	}
	if err := personGate(os.Getenv); err != nil {
		return fail(diag.CodeNoTerminal, err)
	}
	fmt.Fprintf(stdout, repoTrustWarning, termsafe.Field(main), termsafe.Lines(codexLine))
	if !confirmed(stdout, "Trust it?") {
		fmt.Fprintln(stdout, "Not trusted.")
		return 1
	}
	runID, err := applyPatches(ctx, ps)
	if err != nil {
		return fail(diag.CodeTrustBadFile, err)
	}
	fmt.Fprintf(stdout, "Trusted %s. Claude and Grok are not handled yet, so each still asks you itself.\nDone (run %s). aggy unwire --run %s undoes it, once any trust made after it is undone.\n", termsafe.Field(main), runID, runID)
	return 0
}

// codexTrust says what trusting dir does to Codex, and the change, if any.
func codexTrust(dir string) (string, *patch.Patch) {
	path, err := parity.CodexConfigPath()
	if err != nil {
		return "aggy cannot find its config, so nothing changes for it: " + err.Error(), nil
	}
	if fi, err := safefs.Stat(filepath.Dir(path)); err != nil || !fi.IsDir() {
		return codexTrustNone, nil
	}
	trusted, hash, err := parity.CodexTrustState(path, dir)
	switch {
	case errors.Is(err, parity.ErrCodexTrustConflict):
		return path + " already sets another trust level for this folder. That is yours to change, so aggy leaves it.", nil
	case err != nil:
		return "aggy cannot change " + path + ", so nothing changes for it: " + err.Error(), nil
	case trusted:
		return fmt.Sprintf(codexTrustHas, path), nil
	}
	p := parity.CodexTrustPatch(path, dir, hash)
	return fmt.Sprintf(codexTrustWill, path), &p
}

// applyPatches applies changes as one run of the manifest, and returns its id.
func applyPatches(ctx context.Context, ps []patch.Patch) (string, error) {
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		return "", err
	}
	runID := manifest.NewRunID()
	if _, err := m.Run(ctx, manifest.Write, runID, ps, io.Discard); err != nil {
		return "", err
	}
	return runID, nil
}

// keyboard and stdinIsTerminal are the person's side of a change. Tests replace
// them; nothing else does.
var (
	keyboard        io.Reader = os.Stdin
	stdinIsTerminal           = func() bool { return isTerminal(os.Stdin) }
)

// personGate refuses a change a model's shell asks for: trust, permissions, or
// settings. It refuses inside a borrow child, in the tool shell of a session
// aggy runs (which has aggy's own variables even if its stdin were a terminal),
// and with no terminal on stdin. Those are the shapes a mistake takes. It guards
// against mistakes, not a model: a same-user process can clear its variables and
// give itself a pseudo-terminal (docs/design.md, section 10).
func personGate(getenv func(string) string) error {
	if getenv("AGGY_DELEGATED") != "" {
		return errors.New("a borrow child cannot change this; only you can, at a terminal")
	}
	for _, v := range []string{"AGGY_SOCKET", "AGGY_PARENT", "AGGY_VENDOR"} {
		if getenv(v) != "" {
			return errors.New("this looks like a session's own tool shell, and a model can use that; run it yourself at a terminal outside the session")
		}
	}
	if !stdinIsTerminal() {
		return errors.New("this asks you at a terminal, and stdin is not one")
	}
	return nil
}

// confirmed asks, and takes only y or yes.
func confirmed(stdout io.Writer, q string) bool {
	fmt.Fprintf(stdout, "%s [y/N] ", q)
	line, _ := bufio.NewReader(io.LimitReader(keyboard, maxAnswer)).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
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
