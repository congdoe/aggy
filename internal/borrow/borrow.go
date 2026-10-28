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
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// Kinds are the borrow kinds M1 runs. edit comes in M3.
var Kinds = []adapter.Kind{adapter.Image, adapter.Video, adapter.Research, adapter.News, adapter.Review}

// Delegated is the variable that marks a borrow child. Every child runs
// with Delegated=1, and aggy borrow refuses to run under it: one hop.
const Delegated = "AGGY_DELEGATED"

// ErrDelegated is the refusal of a borrow from inside a borrow child.
var ErrDelegated = errors.New("aggy borrow does not run inside a borrow child (" + Delegated + " is set): borrows are one hop")

// ErrEditLater is the refusal of the edit kind.
var ErrEditLater = errors.New("edit comes in M3")

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
}

// CLI is an installed vendor CLI: its absolute path and the
// capabilities its probe found.
type CLI struct {
	Path string
	Caps []string
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
	// Connected reports whether v may take borrows: installed and not
	// known to be signed out.
	Connected func(ctx context.Context, v adapter.Vendor) bool
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
}

// Caps are the concurrency limits of design section 7: at most 2 borrows
// per vendor and 4 in total, across every aggy process (counted through
// store leases).
var Caps = store.BorrowCaps{PerVendor: 2, Total: 4}

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

// ParseKind returns the M1 borrow kind named s. edit is refused with
// ErrEditLater.
func ParseKind(s string) (adapter.Kind, error) {
	k, err := adapter.ParseKind(s)
	if err != nil {
		return "", err
	}
	if k == adapter.Edit {
		return "", ErrEditLater
	}
	return k, nil
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
	vendors, err := b.vendors(ctx, req)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, v := range vendors {
		res, err := b.runOne(ctx, req, v)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// vendors returns the vendors to run req on.
func (b *Borrower) vendors(ctx context.Context, req Request) ([]adapter.Vendor, error) {
	vs, err := b.Router.Choose(ctx, RouteRequest{Kind: req.Kind, Parent: req.Parent, Repo: req.Repo})
	if err != nil {
		return nil, err
	}
	if len(vs) == 0 {
		return nil, fmt.Errorf("no connected vendor can take a %s borrow (run aggy setup)", req.Kind)
	}
	return vs[:1], nil
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
		return Result{}, fmt.Errorf("%s: %w", v, err)
	}
	a := b.Adapter(v, cli)
	budget := b.Config.Budgets.For(req.Kind)
	model := b.Config.Vendors.For(v).Model
	now := b.Now()
	id := newID()
	res := Result{ID: id, Kind: req.Kind, Vendor: v, Model: model}

	dirs, err := b.makeDirs(req)
	if err != nil {
		return Result{}, err
	}
	defer dirs.remove()
	prompt := Prompt{Kind: req.Kind, Task: req.Task, Paths: req.Paths, Constraints: req.Constraints, Today: now}
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
		Env:          []string{Delegated + "=1"},
		GuardCommand: b.Guard,
	}
	cmd, err := a.Headless(hreq)
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
		Model: model, Cwd: dirs.cwd, CreatedAt: now, LastActive: now,
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

	// The time limit: past it the runner kills the child's whole
	// process group.
	timeout := time.Duration(budget.Minutes) * time.Minute
	out, runErr := b.Runner(timeout)(ctx, cmd)
	res.Elapsed = b.Now().Sub(now)
	end := store.BorrowEnd{EndedAt: b.Now()}
	parsed, perr := a.ParseHeadless(hreq, out)
	res.Text, res.Tokens = parsed.Text, parsed.Tokens
	end.VendorSessionID, end.Tokens = parsed.SessionID, parsed.Tokens
	switch {
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
		// Paths in the vendor's own store; nothing is copied here.
		arts, err := a.Artifacts(hreq, parsed)
		if err != nil {
			res.Detail = "listing artifacts: " + err.Error()
		}
		res.Artifacts = arts
		for _, x := range arts {
			end.Artifacts = append(end.Artifacts, x.Path)
			if x.Kind == "image" {
				end.ImageCount++
			}
		}
	}
	path, rerr := b.writeReport(res, req, parsed.SessionID, now)
	if rerr == nil {
		res.ReportPath, end.ReportPath = path, path
	}
	if err := lease.Finish(ctx, end); err != nil {
		return res, err
	}
	if rerr != nil {
		return res, fmt.Errorf("writing the report: %w", rerr)
	}
	return res, nil
}

func classOr(c, def adapter.ErrorClass) adapter.ErrorClass {
	if c == adapter.ErrNone {
		return def
	}
	return c
}
