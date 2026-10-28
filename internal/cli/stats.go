package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"text/tabwriter"
	"time"

	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const statsUsage = `Usage: aggy stats

Shows the finished borrows per kind and vendor: how many ran, how many
ended ok, the average tokens and time of an ok one, and how many ok ones
were redone (the same question asked again, or a rejected edit followed
by another, within 24 hours; see docs/design.md, section 8).

The numbers are what aggy observed of its own borrows. Tokens are what
each CLI reported, not vendor billing.
`

const statsNote = `aggy-observed: tokens are what each CLI reported, not vendor billing.
Averages are over ok borrows. REDONE: ok borrows asked again within 24h.
`

// runStats is aggy stats. Like aggy log, it never creates or writes the
// index.
func runStats(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("stats", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, statsUsage)
			return 0
		}
		fmt.Fprintf(stderr, "aggy stats: %v\n\n%s", err, statsUsage)
		return 2
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "aggy stats: unexpected arguments\n\n%s", statsUsage)
		return 2
	}
	path, err := store.DefaultPath()
	if err != nil {
		fmt.Fprintf(stderr, "aggy stats: %v\n", err)
		return 1
	}
	if _, err := safefs.Stat(path); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(stdout, "No borrows yet.")
		return 0
	} else if err != nil {
		fmt.Fprintf(stderr, "aggy stats: %v\n", err)
		return 1
	}
	ctx := context.Background()
	s, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		fmt.Fprintf(stderr, "aggy stats: %v\n", err)
		return 1
	}
	defer func() { _ = s.Close() }()
	if v, err := s.Version(ctx); err != nil {
		fmt.Fprintf(stderr, "aggy stats: %v\n", err)
		return 1
	} else if v < store.StatsSchema {
		fmt.Fprintf(stdout, "The index is at schema version %d, older than aggy stats reads. Run another aggy command to upgrade it.\n", v)
		return 0
	}
	stats, err := s.BorrowStats(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aggy stats: %v\n", store.ReadError(err))
		return 1
	}
	if len(stats) == 0 {
		fmt.Fprintln(stdout, "No borrows yet.")
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tVENDOR\tRUNS\tOK\tAVG TOKENS\tAVG TIME\tREDONE")
	for _, st := range stats {
		avgTokens, avgTime, redone := "-", "-", "-"
		if st.OK > 0 {
			avgTokens = fmt.Sprint(st.OKTokens / int64(st.OK))
			avgTime = (st.OKTime / time.Duration(st.OK)).Round(time.Second).String()
			redone = fmt.Sprintf("%d of %d (%d%%)", st.Redone, st.OK, st.Redone*100/st.OK)
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%s\t%s\t%s\n", termsafe.Field(st.Kind), termsafe.Field(st.Vendor),
			st.Runs, st.OK, avgTokens, avgTime, redone)
	}
	_ = tw.Flush()
	fmt.Fprint(stdout, "\n"+statsNote)
	return 0
}
