package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/brief"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/transcript"
)

const briefUsage = `Usage: aggy brief --show --from VENDOR --session ID [--to VENDOR] [--turns N]

Previews the handoff brief aggy would give the next CLI: the task file
(.aggy/task.md), the latest turns of the VENDOR session ID, git status and
git diff --stat of the work tree you are in, and the transcript's path.
It calls no model and writes nothing.

  --show          print the brief (required; writing one comes with switching)
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
		// TODO(T6674): write .aggy/briefs/<child-id>.md when switching.
		return usageErr("only --show is implemented")
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
