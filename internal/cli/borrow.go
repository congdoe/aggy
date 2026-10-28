package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/router"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const borrowUsage = `Usage: aggy borrow <kind> "<task>" [--path P]... [--constraint C]... [--vendor V] [--base REF]

Runs one bounded task headless in another vendor's CLI and prints a short
report. The child gets the task, the paths and constraints given here,
and today's date; never this conversation.

Kinds: image, video, research, news, review. (edit comes in M3.)

  --path P         a file or directory the task is about (repeatable)
  --constraint C   a rule for the child, such as "cite primary sources" (repeatable)
  --vendor V       claude, codex, or grok, skipping routing; "both" asks
                   two vendors (research only)
  --base REF       review: diff against REF instead of the merge-base
                   with the default branch
  --allow-untested run a CLI whose version is outside the versions the
                   borrow lockdown was verified on

A review runs at the root of the git work tree you are in. Its child gets
the diff and the --path files in the prompt, as data to review.
`

// borrowEnv is what aggy borrow needs from the outside world, so tests
// can give it fake CLIs.
type borrowEnv struct {
	getenv   func(string) string
	cwd      func() (string, error)
	lookPath func(name string) (string, error)
	// probeRun runs --version and --help for the capability probe.
	probeRun adapter.Runner
	// gitPath finds git, and gitRun runs it, for a review's diff.
	gitPath func() (string, error)
	gitRun  adapter.Runner
	// runner makes the runner for one child.
	runner  func(timeout time.Duration) adapter.Runner
	tempDir string
	now     func() time.Time
	// executable is aggy's own absolute path, for the guard hook.
	executable func() (string, error)
}

// guard is the PreToolUse guard command: this aggy binary, resolved, with
// hook pretooluse. Without a resolvable path there is no guard; the kinds
// M1 runs keep no shell anyway (see internal/borrow).
func (e borrowEnv) guard() []string {
	if e.executable == nil {
		return nil
	}
	exe, err := e.executable()
	if err != nil || !filepath.IsAbs(exe) {
		return nil
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return []string{exe, "hook", "pretooluse"}
}

func defaultBorrowEnv() borrowEnv {
	return borrowEnv{
		getenv:     os.Getenv,
		cwd:        os.Getwd,
		lookPath:   probe.LookPath,
		probeRun:   execrun.ExecRunner(probeTimeout),
		gitPath:    func() (string, error) { return probe.LookPath("git") },
		gitRun:     execrun.ExecRunner(gitTimeout),
		runner:     headlessRunner,
		now:        time.Now,
		executable: os.Executable,
	}
}

// headlessRunner is execrun.HeadlessRunner with its timeout error mapped
// onto borrow.ErrTimeout, so the borrow package never imports execrun.
func headlessRunner(timeout time.Duration) adapter.Runner {
	run := execrun.HeadlessRunner(timeout)
	return func(ctx context.Context, c adapter.Command) (adapter.Output, error) {
		out, err := run(ctx, c)
		if errors.Is(err, execrun.ErrTimeout) {
			err = fmt.Errorf("%w: %v", borrow.ErrTimeout, err)
		}
		return out, err
	}
}

// borrowArgs is a parsed aggy borrow command line.
type borrowArgs struct {
	kind          string
	task          string
	paths         []string
	constraints   []string
	vendor        string
	base          string
	allowUntested bool
	help          bool
}

// parseBorrowArgs reads flags anywhere on the line, so the task can come
// before them. "--" ends the flags.
func parseBorrowArgs(args []string) (borrowArgs, error) {
	var b borrowArgs
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if a == "-h" || a == "--help" {
			b.help = true
			continue
		}
		if a == "--allow-untested" {
			b.allowUntested = true
			continue
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name, val, has := strings.Cut(a, "=")
		if !slices.Contains([]string{"--path", "--constraint", "--vendor", "--base"}, name) {
			return b, fmt.Errorf("unknown flag %s", name)
		}
		if !has {
			if i+1 >= len(args) {
				return b, fmt.Errorf("%s needs a value", name)
			}
			i++
			val = args[i]
		}
		switch name {
		case "--path":
			b.paths = append(b.paths, val)
		case "--constraint":
			b.constraints = append(b.constraints, val)
		case "--vendor":
			b.vendor = val
		case "--base":
			b.base = val
		}
	}
	if b.help {
		return b, nil
	}
	if len(pos) != 2 {
		return b, errors.New(`want a kind and one quoted task`)
	}
	b.kind, b.task = pos[0], pos[1]
	return b, nil
}

func runBorrow(args []string, stdout, stderr io.Writer) int {
	return borrowWith(args, defaultBorrowEnv(), stdout, stderr)
}

func borrowWith(args []string, env borrowEnv, stdout, stderr io.Writer) int {
	// One hop: a borrow child's shell never borrows again.
	if env.getenv(borrow.Delegated) != "" {
		fmt.Fprintf(stderr, "aggy borrow: %s\n", borrow.ErrDelegated)
		return 1
	}
	ba, err := parseBorrowArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "aggy borrow: %s\n\n%s", termsafe.Field(err.Error()), borrowUsage)
		return 2
	}
	if ba.help {
		fmt.Fprint(stdout, borrowUsage)
		return 0
	}
	kind, err := borrow.ParseKind(ba.kind)
	if err != nil {
		fmt.Fprintf(stderr, "aggy borrow: %s\n", termsafe.Field(err.Error()))
		return 2
	}
	cwd, err := env.cwd()
	if err != nil {
		fmt.Fprintf(stderr, "aggy borrow: %v\n", err)
		return 1
	}
	var paths []string
	for _, p := range ba.paths {
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		paths = append(paths, filepath.Clean(p))
	}
	// Ctrl-C, a kill, or a hangup cancels ctx, and the runner then kills
	// the child's whole process group before the outcome is recorded.
	ctx, stop := execrun.NotifyContext(context.Background())
	defer stop()
	cfg, err := config.LoadDefault()
	if err != nil {
		fmt.Fprintf(stderr, "aggy borrow: config: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	// The repo root, never just the cwd: the router reads the repo's
	// config there, and a review runs there.
	root, _ := repoRoot(cwd)
	stateDir, err := home.Dir()
	if err != nil {
		fmt.Fprintf(stderr, "aggy borrow: %v\n", err)
		return 1
	}
	s, err := store.OpenDefault(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aggy borrow: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	defer func() { _ = s.Close() }()

	w := &borrowWorld{env: env, store: s}
	parent, _ := adapter.ParseVendor(env.getenv("AGGY_VENDOR"))
	b := borrow.New(borrow.Deps{
		Store:    s,
		Router:   borrowRouter{router.New(cfg, router.Deps{Statuses: s, Now: env.now})},
		Adapter:  newAdapter,
		Locate:   w.locate,
		Runner:   env.runner,
		Config:   cfg,
		StateDir: stateDir,
		TempDir:  env.tempDir,
		Now:      env.now,
		Getenv:   env.getenv,
		Guard:    env.guard(),
	})
	req := borrow.Request{
		Kind: kind, Task: ba.task, Paths: paths, Constraints: ba.constraints,
		Vendor: ba.vendor, Repo: root, Parent: parent, AllowUntested: ba.allowUntested,
	}
	if kind == adapter.Review {
		if req.Repo, err = reviewRoot(cwd, env.getenv("HOME"), stateDir); err != nil {
			fmt.Fprintf(stderr, "aggy borrow: %s\n", termsafe.Field(err.Error()))
			return 1
		}
		if req.Material, err = env.reviewMaterial(ctx, req.Repo, ba.base, paths); err != nil {
			fmt.Fprintf(stderr, "aggy borrow: review: %s\n", termsafe.Field(err.Error()))
			return 1
		}
	} else if ba.base != "" {
		fmt.Fprintln(stderr, "aggy borrow: --base is for review only")
		return 2
	}
	results, err := b.Run(ctx, req)
	code := 0
	for _, r := range results {
		printResult(stdout, r)
		if r.Status != "done" {
			code = 1
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "aggy borrow: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	return code
}

// maxArtifactLines caps the artifact lines of the short report.
const maxArtifactLines = 3

// printResult prints a short report: a header, up to 8 lines of the
// child's reply (or its error), a few artifacts, and the report's path. Everything from the child goes
// through termsafe: it is untrusted text.
func printResult(w io.Writer, r borrow.Result) {
	model := r.Model
	if model == "" {
		model = "default model"
	}
	fmt.Fprintf(w, "borrow %s: %s on %s (%s): %s in %s, %d tokens\n", r.ID, r.Kind, r.Vendor,
		termsafe.Field(model), r.Outcome, r.Elapsed.Round(time.Second), r.Tokens)
	for _, a := range r.Tried {
		fmt.Fprintf(w, "  passed over %s: %s\n", a.Vendor, termsafe.Field(a.Reason))
	}
	text := r.Text
	if r.Status != "done" {
		text = r.Detail
	}
	for _, l := range borrow.Excerpt(text, 8, 160) {
		fmt.Fprintf(w, "  %s\n", termsafe.Line(l))
	}
	for i, a := range r.Artifacts {
		if i == maxArtifactLines {
			fmt.Fprintf(w, "  and %d more\n", len(r.Artifacts)-i)
			break
		}
		fmt.Fprintf(w, "  %s: %s\n", termsafe.Field(a.Kind), termsafe.Field(a.Path))
	}
	for _, x := range r.Refused {
		fmt.Fprintf(w, "  %s\n", termsafe.Line(x))
	}
	if r.ReportPath != "" {
		fmt.Fprintf(w, "Full report: %s\n", termsafe.Field(r.ReportPath))
	}
}

// newAdapter builds vendor v's adapter for the CLI found.
func newAdapter(v adapter.Vendor, cli borrow.CLI) adapter.Adapter {
	switch v {
	case adapter.Claude:
		return claude.Adapter{Caps: cli.Caps}
	case adapter.Codex:
		return codex.New()
	}
	return grok.New()
}

// borrowWorld answers the borrow package's questions about the CLIs.
type borrowWorld struct {
	env   borrowEnv
	store *store.Store
}

// binName is each vendor's program name.
var binName = map[adapter.Vendor]string{adapter.Claude: claude.Bin, adapter.Codex: codex.Bin, adapter.Grok: grok.Bin}

// locate finds v's CLI on PATH and its capabilities, through the probe
// cache.
func (w *borrowWorld) locate(ctx context.Context, v adapter.Vendor) (borrow.CLI, error) {
	path, err := w.env.lookPath(binName[v])
	if err != nil {
		return borrow.CLI{}, err
	}
	p := &probe.Prober{Store: w.store, Run: w.env.probeRun}
	res, _, err := p.Probe(ctx, v, path)
	if err != nil && !errors.Is(err, probe.ErrIncomplete) {
		return borrow.CLI{}, err
	}
	return borrow.CLI{Path: path, Version: res.Version, Caps: res.Capabilities}, nil
}

// borrowRouter is internal/router behind the borrow package's Router
// interface, whose kind is an adapter.Kind where the router takes a
// string.
type borrowRouter struct{ r *router.Router }

// Choose asks the router, which applies its skip rules to an override
// too: --vendor never bypasses the parent skip, allowed_vendors, or the
// connected check.
func (b borrowRouter) Choose(ctx context.Context, req borrow.RouteRequest) ([]adapter.Vendor, error) {
	return b.r.Choose(ctx, router.Request{Kind: string(req.Kind), Parent: req.Parent, Repo: req.Repo, Override: req.Override})
}

// repoRoot returns the root of the git work tree holding dir (the
// nearest directory with a .git entry) and true, or dir and false when
// dir is in none.
func repoRoot(dir string) (string, bool) {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := safefs.Lstat(filepath.Join(d, ".git")); err == nil {
			return d, true
		}
		if filepath.Dir(d) == d {
			return dir, false
		}
	}
}
