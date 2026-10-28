package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/brief"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/transcript"
)

const briefUsage = `Usage: aggy brief --show --from VENDOR --session ID [--to VENDOR] [--turns N]

Previews the handoff brief aggy would give the next CLI: the task file
(.aggy/task.md), the latest turns of the VENDOR session ID, git status and
git diff --stat of the work tree you are in, and the transcript's path.
It calls no model and writes nothing.

  --show          print the brief (required; aggy resume --brief writes one
                  for the session it opens, and --brief --show previews it)
  --from VENDOR   the CLI being left: claude, codex, or grok
  --session ID    that CLI's session id
  --to VENDOR     the CLI that would read the brief; refused when the
                  repo's allowed_vendors does not list it
  --turns N       how many of the latest turns to include (default 20,
                  at most 1000)
`

// maxTurns caps --turns; the transcript tail is 4 MiB anyway.
const maxTurns = 1000

func runBrief(args []string, stdout, stderr io.Writer) int {
	return briefWith(args, defaultBorrowEnv(), stdout, stderr)
}

func briefWith(args []string, e borrowEnv, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("brief", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	show := fl.Bool("show", false, "")
	from := fl.String("from", "", "")
	to := fl.String("to", "", "")
	session := fl.String("session", "", "")
	turns := fl.Int("turns", 20, "")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, briefUsage)
			return 0
		}
		fmt.Fprintf(stderr, "aggy brief: %v\n\n%s", err, briefUsage)
		return 2
	}
	usageErr := func(msg string) int {
		fmt.Fprintf(stderr, "aggy brief: %s\n\n%s", msg, briefUsage)
		return 2
	}
	switch {
	case fl.NArg() > 0:
		return usageErr("unexpected arguments")
	case !*show:
		return usageErr("--show is required (aggy resume --brief writes a brief)")
	case *turns < 1 || *turns > maxTurns:
		return usageErr(fmt.Sprintf("--turns must be from 1 to %d", maxTurns))
	}
	in := brief.Input{}
	var err error
	if in.From, err = adapter.ParseVendor(*from); err != nil {
		return usageErr("--from: " + err.Error())
	}
	if err := adapter.ValidSessionID(in.From, *session); err != nil {
		return usageErr("--session: " + err.Error())
	}
	cwd, err := e.cwd()
	if err != nil {
		fmt.Fprintf(stderr, "aggy brief: %v\n", err)
		return 1
	}
	root, inRepo := repoRoot(cwd)
	if *to != "" {
		if in.To, err = adapter.ParseVendor(*to); err != nil {
			return usageErr("--to: " + err.Error())
		}
		// A brief carries repo content, so it obeys allowed_vendors.
		rc, err := config.LoadRepo(root)
		if err != nil {
			fmt.Fprintf(stderr, "aggy brief: %v\n", err)
			return 1
		}
		if !config.Merge(config.Config{}, rc).Allows(in.To) {
			fmt.Fprintf(stderr, "aggy brief: this repo's allowed_vendors does not list %s\n", in.To)
			return 1
		}
	}

	in.Task, in.TaskErr = brief.ReadTask(cwd)
	home, err := transcript.Home(in.From, e.getenv)
	if err == nil {
		in.Transcript, err = transcript.Tail(in.From, home, adapter.SessionRef{ID: *session, Cwd: cwd}, *turns)
	}
	in.TranscriptErr = err
	if inRepo {
		in.GitStatus, in.DiffStat, in.GitErr = e.gitState(context.Background(), root)
	} else {
		in.GitErr = errors.New(cwd + " is not in a git work tree")
	}
	// The brief is already escaped for a terminal (brief.Build).
	fmt.Fprint(stdout, brief.Build(in))
	return 0
}

// gitState returns git status --porcelain and git diff --stat for the
// work tree at root, with no program from the repo's config run (gitSafe,
// filterOff) and submodules left alone.
func (e borrowEnv) gitState(ctx context.Context, root string) (status, stat string, err error) {
	off, err := e.filterOff(ctx, root)
	if err != nil {
		return "", "", err
	}
	status, _, err = e.git(ctx, root, append(off, "status", "--porcelain", "--ignore-submodules=all")...)
	if err != nil {
		return "", "", err
	}
	stat, _, err = e.git(ctx, root, append(off, "diff", "--stat", "--no-color", "--no-ext-diff", "--no-textconv", "--ignore-submodules=all", "--")...)
	return status, strings.TrimRight(stat, "\n"), err
}

// childBriefTurns is how many of the latest turns a first visit takes, as
// aggy brief's default.
const childBriefTurns = 20

// childBrief returns the brief for the child pl puts on screen, built
// without a model (T6671). A child seen before gets a return brief: the
// turns the parent's other interactive children added since it was last
// closed, its last-active time in the index. A new child gets the latest
// turns of the child on screen before it. Both carry the task file and
// git state of the child's directory, and the repo's allowed_vendors must
// list the child's vendor, since the brief carries repo content.
func childBrief(ctx context.Context, s *store.Store, e borrowEnv, pl launchPlan) (string, error) {
	to, err := adapter.ParseVendor(pl.child.Vendor)
	if err != nil {
		return "", err
	}
	cwd := pl.child.Cwd
	root, inRepo := repoRoot(cwd)
	rc, err := config.LoadRepo(root)
	if err != nil {
		return "", err
	}
	if !config.Merge(config.Config{}, rc).Allows(to) {
		return "", fmt.Errorf("this repo's allowed_vendors does not list %s, so it gets no brief", to)
	}
	in := brief.Input{To: to}
	in.Task, in.TaskErr = brief.ReadTask(cwd)
	var others []store.Child // most recently active first
	if !pl.newParent {
		cs, err := s.Children(ctx, pl.parent.ID)
		if err != nil {
			return "", err
		}
		for _, c := range cs {
			if c.Kind == "" && c.ID != pl.child.ID && c.VendorSessionID != "" {
				others = append(others, c)
			}
		}
	}
	read := func(c store.Child, turns int) (adapter.Vendor, adapter.Transcript, error) {
		v, err := adapter.ParseVendor(c.Vendor)
		if err != nil {
			return v, adapter.Transcript{}, err
		}
		home := c.VendorHome
		if home == "" {
			if home, err = transcript.Home(v, e.getenv); err != nil {
				return v, adapter.Transcript{}, err
			}
		}
		tr, err := transcript.Tail(v, home, adapter.SessionRef{ID: c.VendorSessionID, Cwd: c.Cwd}, turns)
		return v, tr, err
	}
	switch {
	case pl.newChild && len(others) > 0:
		in.From, in.Transcript, in.TranscriptErr = read(others[0], childBriefTurns)
	case !pl.newChild:
		in.Since = pl.child.LastActive
		var since []brief.Other
		for i := len(others) - 1; i >= 0; i-- {
			if c := others[i]; c.LastActive.After(in.Since) {
				o := brief.Other{}
				o.Vendor, o.Transcript, o.Err = read(c, 0)
				since = append(since, o)
				in.From = o.Vendor
			}
		}
		in.Transcript, in.TranscriptErr = brief.Since(in.Since, since)
	}
	if inRepo {
		in.GitStatus, in.DiffStat, in.GitErr = e.gitState(ctx, root)
	} else {
		in.GitErr = errors.New(cwd + " is not in a git work tree")
	}
	return brief.Build(in), nil
}

// showBrief prints the brief launch would write with t.Brief for the
// child t opens (--brief --show), and launches nothing. It writes no
// brief and no launch to the index; like aggy session, it may raise
// last-active times from the session files (refreshChildren).
func showBrief(name string, t target, stdout, stderr io.Writer) int {
	fail := func(err error) int {
		fmt.Fprintf(stderr, "%s: %s\n", name, termsafe.Line(err.Error()))
		return 1
	}
	ctx := context.Background()
	s, err := openIndex(ctx)
	if err != nil {
		return fail(err)
	}
	if s == nil {
		fmt.Fprintln(stdout, "No session here, so no brief yet. Run aggy to start one.")
		return 0
	}
	defer func() { _ = s.Close() }()
	cfg, err := config.LoadDefault()
	if err != nil {
		return fail(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	pl, err := plan(ctx, s, cfg, cwd, t, time.Now(), stderr)
	if err != nil {
		return fail(err)
	}
	text, err := childBrief(ctx, s, defaultBorrowEnv(), pl)
	if err != nil {
		return fail(err)
	}
	// The brief is already escaped for a terminal (brief.Build).
	fmt.Fprint(stdout, text)
	return 0
}
