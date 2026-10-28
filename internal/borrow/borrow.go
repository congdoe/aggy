package borrow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// Kinds are the borrow kinds aggy borrow runs.
var Kinds = []adapter.Kind{adapter.Image, adapter.Video, adapter.Research, adapter.News, adapter.Review, adapter.Edit}

// Delegated is the variable that marks a borrow child. Every child runs
// with Delegated=1, and aggy borrow refuses to run under it: one hop.
const Delegated = "AGGY_DELEGATED"

// childEnv is a child's added environment: Delegated=1, and the vendor
// and model it runs as (AGGY_VENDOR, AGGY_MODEL when one is set), which
// the attribution hook writes into a commit's trailers.
func childEnv(v adapter.Vendor, model string) []string {
	env := []string{Delegated + "=1", "AGGY_VENDOR=" + string(v)}
	if model != "" {
		env = append(env, "AGGY_MODEL="+model)
	}
	return env
}

// ErrDelegated is the refusal of a borrow from inside a borrow child.
var ErrDelegated = errors.New("aggy borrow does not run inside a borrow child (" + Delegated + " is set): borrows are one hop")

// MaxTask caps the task text: a borrow is one bounded task.
const MaxTask = 64 << 10

// Request is one aggy borrow.
type Request struct {
	Kind adapter.Kind
	// Task is the task text. It goes in the prompt file, never in argv.
	Task string
	// Paths are absolute paths the caller points the child at.
	Paths []string
	// Constraints are extra rules for the child, one per entry.
	Constraints []string
	// Vendor is "" to route, a vendor name to override routing, or
	// "both" (research only) to ask two vendors.
	Vendor string
	// Repo is the absolute directory aggy borrow ran in.
	Repo string
	// Parent is the vendor on screen, from AGGY_VENDOR; "" when unknown.
	Parent adapter.Vendor
	// Authors are the vendors a review's commits name in their
	// Aggy-Vendor trailers: untrusted, so they only steer routing.
	Authors []adapter.Vendor
	// AllowUntested runs a CLI whose version is outside the adapter's
	// tested range (--allow-untested).
	AllowUntested bool
	// Fresh skips the research cache (--fresh). A fresh report still
	// replaces the cached one.
	Fresh bool
	// Material is the review input (required for review, which runs in
	// Repo, the git work tree root); internal/cli gathers it.
	Material *Material
	// Media is the file arguments of aggy image and aggy video; nil for
	// aggy borrow.
	Media *Media
}

// CLI is an installed vendor CLI: its absolute path and the
// capabilities its probe found.
type CLI struct {
	Path    string
	Version string // as the probe read it; "" when unreadable
	// VersionLine is the first --version line when Version is "".
	VersionLine string
	Caps        []string
}

// Deps is what a borrow needs from the outside world. internal/cli fills
// it; tests give it fakes.
type Deps struct {
	Store  *store.Store
	Router Router
	// Adapter returns the adapter for v, built for the CLI found.
	Adapter func(v adapter.Vendor, cli CLI) adapter.Adapter
	// Locate finds v's CLI and its capabilities, or fails when it is not
	// installed.
	Locate func(ctx context.Context, v adapter.Vendor) (CLI, error)
	// Runner returns the process runner for one child, which kills the
	// child's whole process group past timeout and then returns an error
	// matching ErrTimeout.
	Runner func(timeout time.Duration) adapter.Runner
	Config config.Config
	// StateDir is aggy's state directory ($AGGY_HOME or ~/.aggy).
	StateDir string
	// Guard is the PreToolUse guard command (an absolute aggy path, then
	// hook pretooluse), wired into each child where its CLI takes a
	// per-run hook. Empty means no guard.
	Guard []string
	// TempDir is where child dirs are made; "" means the OS temp dir.
	TempDir string
	Now     func() time.Time
	// Getenv reads aggy's own environment, for Delegated. Nil means
	// os.Getenv.
	Getenv func(string) string
	// ChildDone, when set, is called once each child has been reaped
	// (its process group killed if ctx ended), before the outcome is
	// recorded.
	ChildDone func()
	// Worktrees makes an edit child's worktree; nil refuses edit.
	Worktrees Worktrees
}

// Caps are the concurrency limits of design section 7: at most 2 borrows
// per vendor and 4 in total, across every aggy process (counted through
// store leases).
var Caps = store.BorrowCaps{PerVendor: 2, Total: 4}

// Interrupted is the outcome of a borrow whose aggy got SIGINT, SIGTERM,
// or SIGHUP: the child's process group is killed, and the borrow is
// recorded as killed.
const Interrupted = "interrupted"

// ErrTimeout matches a child killed at its time limit. The Runner from
// Deps wraps its own timeout error with it.
var ErrTimeout = errors.New("borrow: time limit reached")

// Result is one finished borrow. Text and Detail are raw child output,
// which a model or a web page may have shaped: print them only through
// termsafe, and never run them.
type Result struct {
	ID         string
	Kind       adapter.Kind
	Vendor     adapter.Vendor
	Model      string
	Status     string // done, failed, or killed
	Outcome    string // "ok" or an adapter.ErrorClass
	Text       string
	Detail     string // the error, when the borrow failed
	Artifacts  []adapter.Artifact
	Tokens     int64
	Elapsed    time.Duration
	ReportPath string
	// ResearchPath is the research or news report (research.go).
	ResearchPath string
	// CachedAt is when a cached report was made (cache.go); zero for a
	// borrow that ran.
	CachedAt time.Time
	// Tried are the candidates passed over before Vendor, and why.
	Tried []Attempt
	// Refused are the files the child left that the artifact gate
	// refused, each with the reason.
	Refused []string
	// Made counts every file an image or video child left in its
	// vendor's store, kept or refused.
	Made int
	// Worktree is an edit child's worktree, kept for aggy accept or
	// aggy reject.
	Worktree store.Worktree
}

// Borrower runs borrows.
type Borrower struct{ Deps }

// New returns a Borrower over d.
func New(d Deps) *Borrower {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Getenv == nil {
		d.Getenv = os.Getenv
	}
	return &Borrower{d}
}

// ParseKind returns the borrow kind named s.
func ParseKind(s string) (adapter.Kind, error) {
	return adapter.ParseKind(s)
}

// validate checks req before anything is recorded.
func (r Request) validate() error {
	if _, err := ParseKind(string(r.Kind)); err != nil {
		return err
	}
	task := strings.TrimSpace(r.Task)
	switch {
	case task == "":
		return errors.New("the task is empty")
	case len(r.Task) > MaxTask:
		return fmt.Errorf("the task is over %d bytes", MaxTask)
	case !filepath.IsAbs(r.Repo):
		return fmt.Errorf("the working dir %q is not absolute", r.Repo)
	case r.Kind == adapter.Review && r.Material == nil:
		return errors.New("a review needs its material (the diff and files)")
	case r.Kind == adapter.Review && !inGitRepo(r.Repo):
		return fmt.Errorf("a review runs in a git work tree, and %s is in none", r.Repo)
	case r.Kind == adapter.Edit && !inGitRepo(r.Repo):
		return fmt.Errorf("an edit gets its own git worktree, and %s is in no git work tree", r.Repo)
	case (r.Kind == adapter.Research || r.Kind == adapter.News) && len(r.Paths) > 0:
		// The child cannot read them, and its queries go to the web, where
		// a local path would leak the user name and layout (#10-13).
		return fmt.Errorf("--path is not for %s: the child cannot read files, and local paths must not reach the web; put what matters in the task", r.Kind)
	}
	for _, p := range r.Paths {
		if !filepath.IsAbs(p) || strings.ContainsAny(p, "\n\r") {
			return fmt.Errorf("path %q must be absolute and on one line", p)
		}
	}
	for _, c := range r.Constraints {
		if strings.ContainsAny(c, "\n\r") {
			return fmt.Errorf("constraint %q must be on one line", c)
		}
	}
	if r.Media != nil {
		return r.Media.validate(r)
	}
	return nil
}

// Run runs the borrow: it picks the vendor, runs the child, and returns
// one Result per vendor asked. A borrow that ran but failed is a Result
// with Status failed or killed, not an error; the error is for a borrow
// that could not start.
func (b *Borrower) Run(ctx context.Context, req Request) ([]Result, error) {
	if b.Getenv(Delegated) != "" {
		return nil, ErrDelegated
	}
	if err := req.validate(); err != nil {
		return nil, err
	}
	if err := b.checkResume(ctx, req); err != nil {
		return nil, err
	}
	if res, ok, err := b.cached(ctx, req); err != nil || ok {
		if err != nil {
			return nil, err
		}
		return []Result{res}, nil
	}
	if req.Vendor == Both {
		vs, err := b.both(ctx, req)
		if err != nil {
			return nil, err
		}
		var out []Result
		for _, v := range vs {
			res, err := b.runOne(ctx, req, v)
			if err != nil {
				return out, err
			}
			out = append(out, res)
		}
		return out, nil
	}
	rr := RouteRequest{Kind: req.Kind, Parent: req.Parent, Repo: req.Repo, Authors: req.Authors}
	if req.Vendor != "" {
		v, err := adapter.ParseVendor(req.Vendor)
		if err != nil {
			return nil, fmt.Errorf("--vendor: %w (or %q for research)", err, Both)
		}
		rr.Override = v
	}
	candidates, hinted, err := b.Router.Choose(ctx, rr)
	if err != nil {
		return nil, err
	}
	// Try the candidates in order. A vendor that cannot start this borrow
	// (its CLI is missing, its adapter refuses the kind, or the lockdown
	// was not verified on its version) is skipped for the next; a run
	// that started and failed is the result. When none could start, the
	// router is asked once more with IgnoreHints, so a limit only a hook
	// reported (which any process the CLI starts can send) never leaves a
	// borrow with no vendor; a confirmed limit still skips. With no vendor
	// left out for a hint, there is nothing to ask again for (F31-2).
	var tried []Attempt
	done := map[adapter.Vendor]bool{}
	for pass := 0; pass < 2; pass++ {
		if pass == 1 {
			if !hinted {
				break
			}
			rr.IgnoreHints = true
			if candidates, _, err = b.Router.Choose(ctx, rr); err != nil {
				break
			}
		}
		for _, v := range candidates {
			if done[v] {
				continue
			}
			done[v] = true
			res, err := b.runOne(ctx, req, v)
			var pre *preStartError
			if errors.As(err, &pre) {
				tried = append(tried, Attempt{Vendor: v, Reason: pre.Error()})
				continue
			}
			res.Tried = tried
			if err != nil {
				return []Result{res}, err
			}
			return []Result{res}, nil
		}
	}
	return nil, &NoStartError{Kind: req.Kind, Tried: tried}
}

// Both is the --vendor value that asks two vendors (research only).
const Both = "both"

// Attempt is a vendor tried before the one that ran, and why it could
// not start the borrow.
type Attempt struct {
	Vendor adapter.Vendor
	Reason string
}

// NoStartError says that no candidate could start the borrow, and why
// each was passed over.
type NoStartError struct {
	Kind  adapter.Kind
	Tried []Attempt
}

func (e *NoStartError) Error() string {
	parts := make([]string, len(e.Tried))
	for i, a := range e.Tried {
		parts[i] = a.Reason
	}
	return fmt.Sprintf("no vendor could start the %s borrow: %s", e.Kind, strings.Join(parts, "; "))
}

// preStartError is a vendor's refusal before anything ran or was
// recorded: the next candidate may try.
type preStartError struct{ err error }

func (e *preStartError) Error() string { return e.err.Error() }
func (e *preStartError) Unwrap() error { return e.err }

// both returns the vendors --vendor both runs on: every vendor the
// router accepts as an override (parent, connected, allowed_vendors)
// whose adapter supports research. There must be two.
func (b *Borrower) both(ctx context.Context, req Request) ([]adapter.Vendor, error) {
	if req.Kind != adapter.Research {
		return nil, fmt.Errorf("--vendor %s is for research only", Both)
	}
	var supported, ready []adapter.Vendor
	for _, v := range adapter.Vendors() {
		// No CLI is probed here, so a missing flag is not held against
		// the kind; runOne checks it on the CLI found.
		if err := Supports(b.Adapter(v, CLI{}), req.Kind); err != nil && !errors.Is(err, adapter.ErrNoCapability) {
			continue
		}
		supported = append(supported, v)
		vs, _, err := b.Router.Choose(ctx, RouteRequest{Kind: req.Kind, Parent: req.Parent, Repo: req.Repo, Override: v})
		if err == nil && len(vs) == 1 {
			ready = append(ready, v)
		}
	}
	switch {
	case len(supported) < 2:
		return nil, fmt.Errorf("--vendor %s: only one vendor supports %s right now (%s)", Both, req.Kind, names(supported))
	case len(ready) < 2:
		return nil, fmt.Errorf("--vendor %s: %s needs two vendors the router accepts, and only %s is", Both, req.Kind, names(ready))
	}
	return ready, nil
}

func names(vs []adapter.Vendor) string {
	if len(vs) == 0 {
		return "none"
	}
	s := make([]string, len(vs))
	for i, v := range vs {
		s[i] = string(v)
	}
	return strings.Join(s, ", ")
}

// newID returns a random id of 16 hex digits.
func newID() string {
	var x [8]byte
	_, _ = rand.Read(x[:])
	return hex.EncodeToString(x[:])
}

// title is the child's title: the task's first line, cut to 60 runes.
func title(task string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(task), "\n")
	r := []rune(strings.TrimSpace(line))
	if len(r) > 60 {
		return string(r[:59]) + "…"
	}
	return string(r)
}

// runOne runs req on vendor v.
func (b *Borrower) runOne(ctx context.Context, req Request, v adapter.Vendor) (Result, error) {
	cli, err := b.Locate(ctx, v)
	if err != nil {
		return Result{}, &preStartError{fmt.Errorf("%s: %w", v, err)}
	}
	a := b.Adapter(v, cli)
	// The adapter may refuse the kind (ErrUnsupported).
	if err := Supports(a, req.Kind); err != nil {
		return Result{}, &preStartError{err}
	}
	// The lockdown flags were verified on the tested versions only: a
	// newer CLI may take them differently, so it needs the caller's
	// say-so.
	if w := adapter.VersionWarning(a, cli.Version, cli.VersionLine); w != "" && !req.AllowUntested {
		return Result{}, &preStartError{fmt.Errorf("%s; the borrow lockdown was verified only there, so aggy will not borrow on it (pass --allow-untested to run it anyway)", w)}
	}
	if err := grokTrustGate(v, req); err != nil {
		return Result{}, &preStartError{err}
	}
	if req.Kind == adapter.Edit {
		if err := editGate(v); err != nil {
			return Result{}, &preStartError{err}
		}
	}
	budget := b.Config.Budgets.For(req.Kind)
	model := b.Config.Vendors.For(v).Model
	now := b.Now()
	id := newID()
	res := Result{ID: id, Kind: req.Kind, Vendor: v, Model: model}
	markRecorded := func() {}

	dirs, err := b.makeDirs(req)
	if err != nil {
		return Result{}, err
	}
	defer dirs.remove()
	prompt := Prompt{Kind: req.Kind, Task: req.Task, Paths: req.Paths, Constraints: req.Constraints, Today: now}
	if req.Kind == adapter.Edit {
		if prompt.Paths, err = editPaths(req); err != nil {
			return Result{}, err
		}
		if b.Worktrees == nil {
			return Result{}, errors.New("edit: no worktree maker")
		}
		wt, err := b.Worktrees.Create(ctx, req.Repo, id)
		if err != nil {
			return Result{}, fmt.Errorf("edit: making the worktree: %w", err)
		}
		// Until the borrow is recorded, a failure removes the worktree;
		// after, it stays for aggy accept or aggy reject.
		recorded := false
		defer func() {
			if !recorded {
				_ = b.Worktrees.Remove(context.WithoutCancel(ctx), wt)
			}
		}()
		markRecorded = func() { recorded = true }
		res.Worktree = wt
		dirs.cwd, dirs.isolated, dirs.gitRepo = wt.Path, false, true
	}
	if req.Kind == adapter.Review {
		prompt.Material = req.Material
	}
	var refs []string
	if req.Media != nil {
		prompt.Constraints = append(slices.Clone(prompt.Constraints), req.Media.constraints()...)
		if refs, err = stageRefs(req.Media.Refs, dirs.run); err != nil {
			return Result{}, err
		}
	}
	promptFile := filepath.Join(dirs.run, "prompt.md")
	if err := safefs.WriteFile(promptFile, []byte(prompt.Text()), 0o600); err != nil {
		return Result{}, err
	}
	hreq := adapter.HeadlessRequest{
		Kind: req.Kind, PromptFile: promptFile, Model: model, Cwd: dirs.cwd,
		IsolatedCwd: dirs.isolated, WebSearch: req.Kind == adapter.Research || req.Kind == adapter.News,
		RunDir: dirs.run, GitRepo: dirs.gitRepo,
		// The turn limit, where the CLI takes one (Grok's --max-turns;
		// Codex has none, and Claude's is not recorded).
		MaxTurns:     budget.Turns,
		Env:          childEnv(v, model),
		GuardCommand: b.Guard,
		RefImages:    refs,
		// The user's opt-in only; see config.Media.
		FileTools: v == adapter.Grok && b.Config.Media.GrokFileTools,
	}
	if req.Media != nil {
		hreq.Resume = req.Media.Resume
	}
	cmd, err := a.Headless(hreq)
	if errors.Is(err, adapter.ErrFileTools) {
		return Result{}, &preStartError{fmt.Errorf("%w; to allow it, set grok_file_tools = true under [media] in %s (Grok then uploads to xAI any local image a path in the brief or the child's reply names)",
			err, filepath.Join(b.StateDir, config.FileName))}
	}
	if errors.Is(err, adapter.ErrUnsupported) {
		// Such as reference images on Grok: the next vendor may serve it.
		return Result{}, &preStartError{err}
	}
	if err != nil {
		return Result{}, err
	}
	if err := writeChildFiles(a, hreq, dirs.run); err != nil {
		return Result{}, err
	}
	cmd.Path = cli.Path

	parent, err := b.Store.ParentForDir(ctx, req.Repo, "borrows", newID(), now)
	if err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256([]byte(req.Task))
	child := store.Child{
		ID: newID(), ParentID: parent.ID, Vendor: string(v), Kind: string(req.Kind), Title: title(req.Task),
		Model: model, Cwd: dirs.cwd, CreatedAt: now, LastActive: now, Worktree: res.Worktree,
	}
	row := store.Borrow{
		ID: id, Kind: string(req.Kind), Vendor: string(v), Model: model,
		TaskHash: hex.EncodeToString(sum[:]), Budget: store.BorrowBudget{Minutes: budget.Minutes, Turns: budget.Turns},
		StartedAt: now,
	}
	lease, err := b.Store.StartBorrow(ctx, child, row, Caps)
	if err != nil {
		return Result{}, err
	}
	markRecorded()

	// The time limit: past it the runner kills the child's whole
	// process group.
	timeout := time.Duration(budget.Minutes) * time.Minute
	// The real wall clock, not b.Now: the artifact gate compares it with
	// the times the file system gives the child's files.
	started := time.Now()
	out, runErr := b.Runner(timeout)(ctx, cmd)
	if b.ChildDone != nil {
		b.ChildDone()
	}
	res.Elapsed = b.Now().Sub(now)
	end := store.BorrowEnd{EndedAt: b.Now()}
	parsed, perr := a.ParseHeadless(hreq, out)
	res.Text, res.Tokens = parsed.Text, parsed.Tokens
	end.VendorSessionID, end.Tokens = parsed.SessionID, parsed.Tokens
	// The run may have ended because aggy was interrupted: what follows
	// still records the outcome and removes the dirs.
	interrupted := ctx.Err() != nil
	ctx = context.WithoutCancel(ctx)
	switch {
	case interrupted:
		res.Status, res.Outcome, res.Detail = "killed", Interrupted, "aggy was interrupted, and the child was killed"
	case errors.Is(runErr, ErrTimeout):
		res.Status, res.Outcome, res.Detail = "killed", string(adapter.ErrTimeout), runErr.Error()
	case runErr != nil:
		res.Status, res.Outcome, res.Detail = "failed", string(adapter.ErrUnknown), runErr.Error()
	case perr != nil:
		res.Status, res.Outcome = "failed", string(classOr(parsed.Error, adapter.ErrUnknown))
		res.Detail = strings.TrimSpace(perr.Error() + "\n" + parsed.ErrorDetail)
	case parsed.Error != adapter.ErrNone:
		res.Status, res.Outcome, res.Detail = "failed", string(parsed.Error), parsed.ErrorDetail
	default:
		res.Status, res.Outcome = "done", "ok"
	}
	end.Status, end.Outcome = res.Status, res.Outcome
	if res.Status == "done" && (req.Kind == adapter.Image || req.Kind == adapter.Video) {
		// Paths in the vendor's own store, each through the artifact
		// gate; nothing is copied here.
		arts, err := a.Artifacts(hreq, parsed)
		if err != nil {
			res.Detail = "listing artifacts: " + err.Error()
		}
		// Every file the vendor left counts, kept or not: image turns use
		// plan limits faster, and Codex can make more than it says.
		passed, refused, made := gate(arts, started, hreq.Resume != "")
		res.Refused, res.Made = refused, made
		if req.Kind == adapter.Image {
			end.ImageCount = res.Made
		}
		for _, m := range passed {
			res.Artifacts = append(res.Artifacts, m.Artifact)
		}
		if req.Media != nil {
			// aggy image and aggy video: the copies are the result.
			var derr error
			if res.Artifacts, derr = deliver(req, res, passed, parsed.SessionID, now); derr != nil {
				res.Status, res.Outcome, res.Detail = "failed", string(adapter.ErrUnknown), strings.TrimSpace(res.Detail+"\n"+derr.Error())
				end.Status, end.Outcome = res.Status, res.Outcome
			}
		}
		for _, x := range res.Artifacts {
			end.Artifacts = append(end.Artifacts, x.Path)
		}
	}
	var serr error
	if res.Status == "done" && web(req.Kind) {
		res.ResearchPath, serr = b.saveResearch(req, res, now)
	}
	path, rerr := b.writeReport(res, req, parsed.SessionID, now)
	if rerr == nil {
		res.ReportPath, end.ReportPath = path, path
	}
	if err := lease.Finish(ctx, end); err != nil {
		return res, err
	}
	if req.Kind == adapter.Edit {
		// Whatever the outcome: the worktree stays for accept or reject.
		tree, err := b.Worktrees.Tree(ctx, res.Worktree)
		if err == nil {
			err = b.Store.SetWorktreeTree(ctx, id, tree)
		}
		if err != nil {
			res.Detail = strings.TrimSpace(res.Detail + "\nrecording the worktree's state: " + err.Error() + " (aggy accept will refuse it)")
		} else {
			res.Worktree.Tree = tree
		}
	}
	// The per-borrow log behind aggy log and, later, aggy stats.
	if err := b.Store.AppendEvent(ctx, store.Event{
		At: end.EndedAt, ParentID: parent.ID, ChildID: child.ID, Type: store.EventBorrow,
		Detail: map[string]any{
			"borrow": id, "kind": string(req.Kind), "vendor": string(v), "model": model,
			"tokens": res.Tokens, "ms": res.Elapsed.Milliseconds(), "outcome": res.Outcome,
		},
	}); err != nil {
		return res, err
	}
	if rerr != nil {
		return res, fmt.Errorf("writing the report: %w", rerr)
	}
	if serr != nil {
		return res, fmt.Errorf("writing the %s report: %w", req.Kind, serr)
	}
	return res, nil
}

func classOr(c, def adapter.ErrorClass) adapter.ErrorClass {
	if c == adapter.ErrNone {
		return def
	}
	return c
}
