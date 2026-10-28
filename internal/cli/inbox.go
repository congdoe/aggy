package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const inboxUsage = `Usage: aggy inbox [--all] [ID]

Lists the borrow reports you have not read, newest first. With an ID (or
a unique prefix of one), shows that report and marks it read.

  --all   list read reports too
`

// maxReport caps a report shown by aggy inbox.
const maxReport = 4 << 20

// minPrefix is the shortest id prefix aggy inbox takes.
const minPrefix = 4

func runInbox(args []string, stdout, stderr io.Writer) int {
	return inboxWith(args, time.Now, stdout, stderr)
}

func inboxWith(args []string, now func() time.Time, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("inbox", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	all := fl.Bool("all", false, "")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, inboxUsage)
			return 0
		}
		fmt.Fprintf(stderr, "aggy inbox: %v\n\n%s", err, inboxUsage)
		return 2
	}
	if fl.NArg() > 1 {
		fmt.Fprintf(stderr, "aggy inbox: unexpected arguments\n\n%s", inboxUsage)
		return 2
	}
	path, err := store.DefaultPath()
	if err != nil {
		fmt.Fprintf(stderr, "aggy inbox: %v\n", err)
		return 1
	}
	if _, err := safefs.Stat(path); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(stdout, "No reports yet.")
		return 0
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "aggy inbox: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	defer func() { _ = s.Close() }()
	if fl.NArg() == 1 {
		return showReport(ctx, s, fl.Arg(0), now, stdout, stderr)
	}
	rows, err := s.InboxBorrows(ctx, *all)
	if err != nil {
		fmt.Fprintf(stderr, "aggy inbox: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	if len(rows) == 0 {
		if *all {
			fmt.Fprintln(stdout, "No reports yet.")
		} else {
			fmt.Fprintln(stdout, "No unread reports. aggy inbox --all lists read ones.")
		}
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTARTED (UTC)\tKIND\tVENDOR\tOUTCOME\tTASK")
	for _, b := range rows {
		id := b.ID
		if *all && b.ReadAt.IsZero() {
			id += " *"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", termsafe.Field(id), b.StartedAt.UTC().Format("2006-01-02 15:04"),
			termsafe.Field(b.Kind), termsafe.Field(b.Vendor), termsafe.Field(b.Outcome), termsafe.Field(b.Title))
	}
	_ = tw.Flush()
	if *all {
		fmt.Fprintln(stdout, "* unread")
	}
	return 0
}

// showReport prints the report of the borrow whose id starts with
// prefix and marks it read. The report holds child output, so every
// line goes through termsafe. It is read from the inbox path the id
// names, never from a path stored in the row.
func showReport(ctx context.Context, s *store.Store, prefix string, now func() time.Time, stdout, stderr io.Writer) int {
	prefix = strings.ToLower(prefix)
	if len(prefix) < minPrefix || len(prefix) > 16 || strings.Trim(prefix, "0123456789abcdef") != "" {
		fmt.Fprintf(stderr, "aggy inbox: %q is not a borrow id (at least %d hex digits)\n", termsafe.Field(prefix), minPrefix)
		return 2
	}
	rows, err := s.BorrowsByPrefix(ctx, prefix)
	if err != nil {
		fmt.Fprintf(stderr, "aggy inbox: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	switch {
	case len(rows) == 0:
		fmt.Fprintf(stderr, "aggy inbox: no borrow %s\n", prefix)
		return 1
	case len(rows) > 1:
		fmt.Fprintf(stderr, "aggy inbox: %s matches %d borrows; give more of the id\n", prefix, len(rows))
		return 1
	}
	b := rows[0]
	if b.ReportPath == "" {
		fmt.Fprintf(stderr, "aggy inbox: borrow %s has no report (status %s)\n", b.ID, termsafe.Field(b.Status))
		return 1
	}
	dir, err := home.Dir()
	if err != nil {
		fmt.Fprintf(stderr, "aggy inbox: %v\n", err)
		return 1
	}
	data, err := safefs.ReadFileLimit(borrow.ReportPath(dir, b.ID), maxReport)
	if err != nil {
		fmt.Fprintf(stderr, "aggy inbox: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	fmt.Fprintln(stdout, termsafe.Lines(strings.TrimRight(string(data), "\n")))
	if err := s.MarkBorrowRead(ctx, b.ID, now()); err != nil {
		fmt.Fprintf(stderr, "aggy inbox: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	return 0
}
