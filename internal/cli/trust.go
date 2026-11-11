package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
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
	"github.com/congdoe/aggy/internal/trustlist"
)

const trustUsage = `Usage: aggy trust <dir>
       aggy trust --opencode <dir>

aggy trust <dir> shows the trust list for the folder holding <dir>, the one aggy
asks the first time it starts there: what may run in it (Claude Code, OpenCode)
and where its code may be sent (OpenCode's free models, when they are on in your
config). A harness not installed is listed, marked so. Ticking one trusts the
repo for aggy's rules: your allow rules beyond the known read-only ones apply
there, those in its .claude/settings.local.json and your approvals from Codex
among them, and Codex loads it as trusted. Claude Code ticked also records
Claude's own trust for the folder, so Claude does not ask. OpenCode ticked lets
the repo open the full OpenCode screen under aggy, its own plugins included.
Rows you leave unticked count as off, and aggy does not ask about them again.

aggy trust --opencode <dir> is the shortcut that ticks OpenCode alone.

Each asks at a terminal, and refuses in a session's own tool shell. That holds
against mistakes, not a model that tries: it can give itself a terminal with
script, and type an answer. Each records the repo root, so a subfolder is treated
the same. A repo cannot trust itself: this is kept in aggy's own state folder. aggy
unwire undoes it. Your home folder and / cannot be trusted.

  --opencode   trust it for OpenCode
  --dry-run    show what ticking every harness changes, and change nothing
`

// trustWarning says what trusting a repo for OpenCode means. It is the
// design's own words (docs/design.md, section 15).
const trustWarning = `Trust %s for OpenCode?

- The repo's screen plugins (.opencode/plugins/*/tui.js) WILL run, as you, before
  any guard, each time the full screen opens. No setting stops them.
- Reads in this repo run with no prompt, so a link in the repo can reach a file
  outside it; aggy's guard refuses what it can see, and only that. grep's search
  text is not covered: a deny by file name applies to the path it searches.
- Your allow rules beyond the known read-only ones apply here on every CLI aggy
  passes them to, those in this repo's .claude/settings.local.json and your
  approvals from Codex among them.
- aggy gives guardrails against mistakes, not protection from a hostile repo or a
  tricked model. A model's shell runs as you with no OS sandbox.
- A key you stored with aggy key is in a file while OpenCode runs, and any process
  running as you, the repo's plugins and other sessions' shells among them, can
  read it then.

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
	root, _ := repoRoot(dir)
	if err := tooBroadToTrust(dir, root); err != nil {
		return fail(diag.CodeTrustBadDir, err)
	}
	if !*forOpenCode {
		return trustListCmd(ctx, dir, *dryRun, fail, stdout, stderr)
	}
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
	// The shortcut ticks the list's OpenCode row: its answer is recorded with
	// the rest of the folder's, so a start does not ask about it again.
	ps := []patch.Patch{p}
	ap, err := openCodeAnswerPatch(dir)
	if err != nil {
		return fail(diag.CodeTrustBadFile, err)
	}
	if ap != nil {
		ps = append([]patch.Patch{*ap}, ps...)
	}
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

// trustListCmd is aggy trust <dir>: the folder's trust list, as a start shows
// it, with every harness listed (one not installed marked so). --dry-run shows
// what ticking every harness changes, and changes nothing.
func trustListCmd(ctx context.Context, dir string, dryRun bool, fail func(diag.Code, error) int, stdout, stderr io.Writer) int {
	cfg, err := config.LoadDefault()
	if err != nil {
		return fail(diag.CodeConfig, err)
	}
	tg, err := trustTargetFor(dir)
	if err != nil {
		return fail(diag.CodeTrustBadDir, err)
	}
	l, err := openTrustList(tg, cfg, true)
	if err != nil {
		return fail(diag.CodeTrustBadFile, err)
	}
	if dryRun {
		if !l.st.AllOn() {
			l.st.All()
		}
		fmt.Fprintln(stdout, "With every harness ticked:")
		l.st.Write(stdout)
		return applyTrustList(ctx, l, false, true, true, stdout, stderr)
	}
	if err := personGate(os.Getenv); err != nil {
		return fail(diag.CodeNoTerminal, err)
	}
	switch askTrustList(l, l.header(false), keyboard, stdout) {
	case trustlist.Confirm:
		return applyTrustList(ctx, l, false, true, false, stdout, stderr)
	case trustlist.Never:
		return applyTrustList(ctx, l, true, true, false, stdout, stderr)
	}
	fmt.Fprintln(stdout, "Not trusted: nothing changed.")
	return 1
}

// openCodeAnswerPatch records the OpenCode row ticked in dir's answer, the
// rest kept; nil when it says so already.
func openCodeAnswerPatch(dir string) (*patch.Patch, error) {
	tg, err := trustTargetFor(dir)
	if err != nil {
		return nil, err
	}
	path, err := trustFile()
	if err != nil {
		return nil, err
	}
	f, err := readTrustRecord(path)
	if err != nil {
		return nil, err
	}
	ans := f.Folders[tg.key]
	for _, l := range []*[]string{&ans.Shown, &ans.On} {
		if !slices.Contains(*l, trustRowOpenCode) {
			*l = append(slices.Clone(*l), trustRowOpenCode)
		}
	}
	p, changes, err := trustFileChange(path, tg.key, 0, tg.key, &ans)
	if err != nil || !changes {
		return nil, err
	}
	return &p, nil
}

// tooBroadToTrust refuses / and the home folder, or a repo at either: trust there
// would cover everything the person has.
func tooBroadToTrust(paths ...string) error {
	home := ""
	if h := os.Getenv("HOME"); h != "" {
		if real, err := safefs.RealPath(h); err == nil {
			home = real
		}
	}
	for _, p := range paths {
		if p == "/" || (p != "" && p == home) {
			return fmt.Errorf("%s holds too much to trust as one repo; name the repo itself", p)
		}
	}
	return nil
}

const (
	codexTrustWill = "%s gains trust_level = \"trusted\" for this folder, so Codex\n  loads the repo's own config, hooks, and rules. (aggy takes this to be Codex's\n  key; it has not been checked against a live Codex.)"
	codexTrustHas  = "%s already trusts this folder."
	codexTrustNone = "no Codex config folder here, so nothing changes for it."
)

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

// shellGate refuses a change a model could make for itself from the session's
// own tool shell (R121): accept, reject, and tool on|off. It refuses in a
// borrow child, and in a shell with a session's variables and no terminal,
// which is how a model's tool shell looks; a person at a terminal, or aggy's
// own in-session page (no such variables in aggy's process), passes. Like
// personGate it guards against mistakes, not a determined model.
//
// TODO(T6975): it refuses a person's own `!aggy accept` from a CLI's shell
// mode too, if stdin is not a terminal there (unverified), and unsetting the
// AGGY_* variables gets past it.
func shellGate(getenv func(string) string, tty bool) error {
	if getenv("AGGY_DELEGATED") != "" {
		return withCode(diag.CodeNoTerminal, errors.New("a borrow child cannot change this; only you can"))
	}
	if tty {
		return nil
	}
	for _, v := range []string{"AGGY_SOCKET", "AGGY_PARENT", "AGGY_VENDOR"} {
		if getenv(v) != "" {
			return withCode(diag.CodeNoTerminal, errors.New("this looks like a session's own tool shell with no terminal, and a model can use that; type it at the prompt (aggy catches it there and asks first), or run it yourself at a terminal"))
		}
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

// trustPatch adds key to the repos of the trust file at path, or reports it
// is there. The rest of the file (the trust list's answers) is kept.
func trustPatch(path, key string) (patch.Patch, bool, error) {
	p, changes, err := trustRepoPatch(path, key, true)
	return p, !changes && err == nil, err
}
