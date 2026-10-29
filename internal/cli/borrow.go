package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/limits"
	"github.com/congdoe/aggy/internal/notify"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/router"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/tooldef"
)

const borrowUsage = `Usage: aggy borrow <kind> "<task>" [--path P]... [--constraint C]... [--vendor V] [--base REF] [--fresh] [--tool NAME]...

Runs one bounded task headless in a vendor's CLI (for review, possibly the
one on screen, in a fresh session) and prints a short report. The child gets the task, the paths and constraints given here,
and today's date; never this conversation.

Kinds: image, video, research, news, review, edit.

  --path P         a file or directory the task is about (repeatable)
  --constraint C   a rule for the child, such as "cite primary sources" (repeatable)
  --vendor V       claude, codex, or grok, skipping routing; "both" asks
                   two vendors (research only)
  --base REF       review: diff against REF instead of the merge-base
                   with the default branch
  --allow-untested run a CLI whose version is outside the versions the
                   borrow lockdown was verified on
  --fresh          research and news: ask again instead of reusing the
                   report on the same question (a day for news, a week
                   for research)
  --tool NAME      edit: give the child this managed tool's MCP server
                   (repeatable); it must be listed in routing.edit_tools
                   in ~/.aggy/config.toml, which an edit gets by default

A review runs at the root of the git work tree you are in. Its child gets
the diff and the --path files in the prompt, as data to review.

An edit runs in a git worktree of its own, on a new branch aggy/edit-<id>
made from HEAD, under aggy's state dir; it never writes this checkout,
and its CLI's own sandbox stays on. aggy check <id> runs the repo's
checks on it once you approve, aggy accept <id> applies its diff here,
and aggy reject <id> discards it.
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
	// setupRun runs a repo's worktree setup command, once the person
	// allowed it.
	setupRun adapter.Runner
	// runner makes the runner for one child.
	runner  func(timeout time.Duration) adapter.Runner
	tempDir string
	now     func() time.Time
	// executable is aggy's own absolute path, for the guard hook.
	executable func() (string, error)
	// kernelSandbox is probe.HostKernelSandbox: nil when the host can
	// apply Grok's sandbox. A nil func means it cannot.
	kernelSandbox func() error
	// tmpDirs are the temp dirs an edit child's sandbox may write; nil
	// means defaultTmpDirs.
	tmpDirs func() []string
}

// defaultTmpDirs are /tmp and $TMPDIR, which Codex's workspace-write
// sandbox leaves writable by default (verified live), and the OS temp
// dir. codex.EditLockdown excludes them; refusing them here too is
// defense in depth for older Codex versions and other sandboxes.
func defaultTmpDirs() []string {
	return []string{"/tmp", "/private/tmp", os.Getenv("TMPDIR"), os.TempDir()}
}

// childWritable returns the temp dir that holds p (by real path), or ""
// when none does. On darwin, whose default file system ignores case, so
// does the comparison.
func (e borrowEnv) childWritable(p string) string {
	dirs := defaultTmpDirs
	if e.tmpDirs != nil {
		dirs = e.tmpDirs
	}
	fold := func(s string) string { return s }
	if runtime.GOOS == "darwin" {
		fold = strings.ToLower
	}
	r, err := realPath(p)
	if err != nil {
		r = resolvePath(p)
	}
	r = fold(r)
	for _, d := range dirs() {
		if d != "" && within(r, fold(resolvePath(d))) {
			return d
		}
	}
	return ""
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
		getenv:   os.Getenv,
		cwd:      os.Getwd,
		lookPath: probe.LookPath,
		probeRun: execrun.ExecRunner(probeTimeout),
		// Checked per borrow, never cached: the kernel can change.
		kernelSandbox: probe.HostKernelSandbox,
		gitPath:       func() (string, error) { return probe.LookPath("git") },
		gitRun:        execrun.ExecRunner(gitTimeout),
		setupRun:      execrun.ExecRunner(setupTimeout),
		runner:        headlessRunner,
		now:           time.Now,
		executable:    os.Executable,
		tmpDirs:       defaultTmpDirs,
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
	fresh         bool
	help          bool
	tools         []string
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
		if a == "--fresh" {
			b.fresh = true
			continue
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name, val, has := strings.Cut(a, "=")
		if !slices.Contains([]string{"--path", "--constraint", "--vendor", "--base", "--tool"}, name) {
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
		case "--tool":
			if !tooldef.ValidName(val) {
				return b, fmt.Errorf("--tool %q is not a tool name", val)
			}
			if !slices.Contains(b.tools, val) {
				b.tools = append(b.tools, val)
			}
			if len(b.tools) > config.MaxBorrowTools {
				return b, fmt.Errorf("more than %d --tool", config.MaxBorrowTools)
			}
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
	return borrowMedia(args, nil, env, stdout, stderr)
}

// borrowMedia is aggy borrow with the file arguments of aggy image or
// aggy video (nil for aggy borrow).
func borrowMedia(args []string, media *borrow.Media, env borrowEnv, stdout, stderr io.Writer) int {
	bg := context.Background()
	fail := func(code diag.Code, msg string) int {
		refuse(bg, stderr, "borrow", diag.EvBorrowRefused, code, msg, diag.Rec{})
		return 1
	}
	failErr := func(code diag.Code, err error) int {
		refuse(bg, stderr, "borrow", diag.EvBorrowRefused, code, err.Error(), errRec(diag.Rec{}, err))
		return 1
	}
	// One hop: a borrow child's shell never borrows again.
	if env.getenv(borrow.Delegated) != "" {
		return fail(diag.CodeOneHop, borrow.ErrDelegated.Error())
	}
	ba, err := parseBorrowArgs(args)
	if err != nil {
		failErr(diag.CodeUsage, err)
		fmt.Fprintf(stderr, "\n%s", borrowUsage)
		return 2
	}
	if ba.help {
		fmt.Fprint(stdout, borrowUsage)
		return 0
	}
	// The task and constraints never reach the diagnostic log.
	dlog.Forbid(append([]string{ba.task}, ba.constraints...)...)
	kind, err := borrow.ParseKind(ba.kind)
	if err != nil {
		failErr(diag.CodeUsage, err)
		return 2
	}
	cwd, err := env.cwd()
	if err != nil {
		return failErr(diag.CodeBorrowStart, err)
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
		return failErr(diag.CodeConfig, fmt.Errorf("config: %w", err))
	}
	// The repo root, never just the cwd: the router reads the repo's
	// config there, and a review runs there.
	root, _ := repoRoot(cwd)
	stateDir, err := home.Dir()
	if err != nil {
		return failErr(diag.CodeStore, err)
	}
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return failErr(diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()

	w := &borrowWorld{env: env, store: s}
	parent, _ := adapter.ParseVendor(env.getenv("AGGY_VENDOR"))
	b := borrow.New(borrow.Deps{
		Store:    s,
		Router:   borrowRouter{router.New(cfg, router.Deps{Statuses: s, Limits: limits.State{Store: s}, Now: env.now}), stderr},
		Adapter:  newAdapter,
		Locate:   w.locate,
		Runner:   env.runner,
		Config:   cfg,
		StateDir: stateDir,
		TempDir:  env.tempDir,
		Now:      env.now,
		Getenv:   env.getenv,
		Guard:    env.guard(),
		// Once a signal has cancelled ctx and the child is reaped (its
		// process group killed), a second signal takes its default action
		// and ends aggy during cleanup (R-5). Not before: aggy must not
		// end while the child still runs (C17-7).
		ChildDone: func() {
			if ctx.Err() != nil {
				stop()
			}
		},
		Worktrees: &gitWorktrees{env: env, stateDir: stateDir, out: stderr, allow: cfg.Worktree, cfgPath: filepath.Join(stateDir, config.FileName)},
		Tools:     borrowTools(stateDir, cfg.Routing.EditNetworkTools),
		Diag:      dlog,
	})
	req := borrow.Request{
		Kind: kind, Task: ba.task, Paths: paths, Constraints: ba.constraints,
		Vendor: ba.vendor, Repo: root, Parent: parent, AllowUntested: ba.allowUntested, Media: media,
	}
	if kind == adapter.Review {
		if req.Repo, err = reviewRoot(cwd, env.getenv, stateDir); err != nil {
			return failErr(diag.CodeBorrowInvalid, err)
		}
		if req.Material, err = env.reviewMaterial(ctx, req.Repo, ba.base, paths); err != nil {
			return failErr(diag.CodeBorrowStart, fmt.Errorf("review: %w", err))
		}
		if links, err := env.cursorLinks(ctx, req.Repo); err == nil { // else Grok is refused (CursorChecked false)
			req.CursorLinks, req.CursorChecked = links, true
		}
		if cfg.Routing.Review.NotAuthor { // only that route reads trailers; skip the history walk otherwise
			req.Authors = env.reviewAuthors(ctx, req.Repo, req.Material.Base)
		}
	} else if ba.base != "" {
		fail(diag.CodeUsage, "--base is for review only")
		return 2
	}
	if ba.fresh && kind != adapter.Research && kind != adapter.News {
		fail(diag.CodeUsage, "--fresh is for research and news only")
		return 2
	}
	req.Fresh = ba.fresh
	tools, c, err := editTools(kind, ba.tools, cfg.Routing)
	if c == diag.CodeToolBorrowKind {
		failErr(c, err)
		return 2
	}
	if err != nil {
		return failErr(c, err)
	}
	req.Tools = tools
	results, err := b.Run(ctx, req)
	code := 0
	for _, r := range results {
		printResult(stdout, r)
		if r.Status != "done" {
			code = 1
		}
	}
	if err != nil {
		c := diag.CodeBorrowStart
		var none *borrow.NoStartError
		switch {
		case errors.Is(err, borrow.ErrDelegated):
			c = diag.CodeOneHop
		case borrow.Invalid(err):
			c = diag.CodeBorrowInvalid
		case errors.As(err, &none):
			c = diag.CodeNoVendor
		}
		// Its text may quote a path or constraint as typed, or a vendor's
		// own words, so the record holds the code's text (G67-1).
		rec := diag.Rec{Event: diag.EvBorrowRefused, Code: c, Msg: fixedMsg(c)}
		dlog.Log(ctx, rec)
		fmt.Fprintf(stderr, "aggy borrow: %s %s\n", termsafe.Field(err.Error()), dlog.Tag(c))
		code = 1
	}
	// Record and notify once the report is out: the notifier can take
	// seconds.
	nctx := context.WithoutCancel(ctx)
	n := newNotifier(cfg)
	recordBorrowLimits(nctx, s, results, env.now(), n)
	if len(results) > 0 {
		r := results[len(results)-1]
		n.Show(nctx, "borrow", "aggy", borrowText(r.Kind, r.Vendor, r.Outcome))
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
	if !r.CachedAt.IsZero() {
		fmt.Fprintf(w, "borrow %s: %s on %s (%s): %s, cached from %s (--fresh asks again)\n", r.ID, r.Kind, r.Vendor,
			termsafe.Field(model), r.Outcome, r.CachedAt.UTC().Format("2006-01-02 15:04 UTC"))
	} else {
		tag := ""
		if r.Status != "done" {
			tag = " " + dlog.Tag(borrow.OutcomeCode(r.Outcome))
		}
		fmt.Fprintf(w, "borrow %s: %s on %s (%s): %s in %s, %d tokens%s\n", r.ID, r.Kind, r.Vendor,
			termsafe.Field(model), r.Outcome, r.Elapsed.Round(time.Second), r.Tokens, tag)
	}
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
	if r.Status == "done" && (r.Kind == adapter.Image || r.Kind == adapter.Video) {
		fmt.Fprintf(w, "  %d files made, %d kept\n", r.Made, len(r.Artifacts))
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
	if wt := r.Worktree; wt.Path != "" {
		fmt.Fprintf(w, "  worktree %s on branch %s\n", termsafe.Field(wt.Path), termsafe.Field(wt.Branch))
		// The checks wait for the person (W-5): they run the child's code.
		// TODO(T6691): send a failed check's output back to the edit child
		// as its next message; that needs a resume of the edit child.
		fmt.Fprintf(w, "  aggy check %s shows the diff stat and, once you say y, runs the repo's checks on it\n", r.ID)
		fmt.Fprintf(w, "  aggy accept %s shows the diff and the checks' result and applies it to %s; aggy reject %s discards it\n", r.ID, termsafe.Field(wt.Repo), r.ID)
	}
	if r.ResearchPath != "" {
		fmt.Fprintf(w, "Report: %s\n", termsafe.Field(r.ResearchPath))
	} else if r.ReportPath != "" {
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
	return grok.Adapter{Caps: cli.Caps}
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
	caps := res.Capabilities
	if v == adapter.Grok && w.env.kernelSandbox != nil && w.env.kernelSandbox() == nil {
		caps = append(slices.Clone(caps), probe.CapKernelSandbox)
	}
	return borrow.CLI{Path: path, Version: res.Version, VersionLine: res.VersionLine, Caps: caps}, nil
}

// borrowRouter is internal/router behind the borrow package's Router
// interface, whose kind is an adapter.Kind where the router takes a
// string. The router's warnings go to warn.
type borrowRouter struct {
	r    *router.Router
	warn io.Writer
}

// Choose asks the router, which applies its skip rules to an override
// too: --vendor never bypasses the parent skip (where the kind has one;
// a review allows the parent unless the config opts in), allowed_vendors,
// or the connected check.
func (b borrowRouter) Choose(ctx context.Context, req borrow.RouteRequest) ([]adapter.Vendor, bool, error) {
	d, err := b.r.Decide(ctx, router.Request{Kind: string(req.Kind), Parent: req.Parent, Repo: req.Repo, Override: req.Override, Authors: req.Authors, IgnoreHints: req.IgnoreHints})
	if err != nil {
		return nil, false, err
	}
	for _, w := range d.Warnings {
		fmt.Fprintf(b.warn, "aggy borrow: warning: %s\n", termsafe.Field(w))
	}
	return d.Candidates, d.HintSkipped, nil
}

// recordBorrowLimits records a limit for each vendor whose child failed
// with rate_limit, and notifies it through n (nil shows nothing). The
// class comes from the vendor CLI's own headless output, which aggy ran,
// so it is not a hint. A failed write loses the signal, not the borrow.
func recordBorrowLimits(ctx context.Context, a limits.Appender, results []borrow.Result, now time.Time, n *notify.Notifier) {
	for _, r := range results {
		if r.Outcome != string(adapter.ErrRateLimit) {
			continue
		}
		sig := limits.Signal{Vendor: r.Vendor, Via: limits.ViaBorrow}
		if until, err := limits.Record(ctx, a, sig, "", now); err == nil {
			showLimit(ctx, n, sig, until)
		}
	}
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
