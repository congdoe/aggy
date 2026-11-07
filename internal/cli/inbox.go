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
	"github.com/congdoe/aggy/internal/diag"
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
		return cmdUsage(stderr, "inbox", err.Error(), inboxUsage)
	}
	if fl.NArg() > 1 {
		return cmdUsage(stderr, "inbox", "unexpected arguments", inboxUsage)
	}
	path, err := store.DefaultPath()
	if err != nil {
		return cmdErr(stderr, "inbox", diag.CodeStore, err)
	}
	if _, err := safefs.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if fl.NArg() == 1 {
			// An id names a report, and with no index there is none.
			if code, ok := checkBorrowPrefix(strings.ToLower(fl.Arg(0)), stderr); !ok {
				return code
			}
			return cmdFail(stderr, "inbox", diag.CodeNotFound, "no borrow "+strings.ToLower(fl.Arg(0)))
		}
		fmt.Fprintln(stdout, "No reports yet.")
		return 0
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return cmdErr(stderr, "inbox", diag.CodeStore, err)
	}
	defer func() { _ = s.Close() }()
	if fl.NArg() == 1 {
		return showReport(ctx, s, fl.Arg(0), now, stdout, stderr)
	}
	rows, err := s.InboxBorrows(ctx, *all)
	if err != nil {
		return cmdErr(stderr, "inbox", diag.CodeStore, err)
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

// checkBorrowPrefix refuses what cannot be the start of a borrow id.
func checkBorrowPrefix(prefix string, stderr io.Writer) (int, bool) {
	if len(prefix) < minPrefix || len(prefix) > 16 || strings.Trim(prefix, "0123456789abcdef") != "" {
		return cmdUsage(stderr, "inbox", fmt.Sprintf("%q is not a borrow id (at least %d hex digits)", prefix, minPrefix), ""), false
	}
	return 0, true
}

// showReport prints the report of the borrow whose id starts with
// prefix and marks it read. The report holds child output, so every
// line goes through termsafe. It is read from the inbox path the id
// names, never from a path stored in the row.
func showReport(ctx context.Context, s *store.Store, prefix string, now func() time.Time, stdout, stderr io.Writer) int {
	prefix = strings.ToLower(prefix)
	if code, ok := checkBorrowPrefix(prefix, stderr); !ok {
		return code
	}
	rows, err := s.BorrowsByPrefix(ctx, prefix)
	if err != nil {
		return cmdErr(stderr, "inbox", diag.CodeStore, err)
	}
	switch {
	case len(rows) == 0:
		return cmdFail(stderr, "inbox", diag.CodeNotFound, "no borrow "+prefix)
	case len(rows) > 1:
		return cmdFail(stderr, "inbox", diag.CodeAmbiguousID, fmt.Sprintf("%s matches %d borrows; give more of the id", prefix, len(rows)))
	}
	b := rows[0]
	// The id is joined into a path below: it must be exactly what aggy
	// makes, whatever the row holds.
	if !borrow.ValidID(b.ID) {
		return cmdFail(stderr, "inbox", diag.CodeStore, fmt.Sprintf("borrow id %s is not 16 hex digits", b.ID))
	}
	if b.ReportPath == "" {
		return cmdFail(stderr, "inbox", diag.CodeNotFound, fmt.Sprintf("borrow %s has no report (status %s)", b.ID, b.Status))
	}
	dir, err := home.Dir()
	if err != nil {
		return cmdErr(stderr, "inbox", diag.CodeStore, err)
	}
	data, err := safefs.ReadFileLimit(borrow.ReportPath(dir, b.ID), maxReport)
	if err != nil {
		return cmdErr(stderr, "inbox", diag.CodeStore, err)
	}
	fmt.Fprintln(stdout, termsafe.Lines(strings.TrimRight(string(data), "\n")))
	if err := s.MarkBorrowRead(ctx, b.ID, now()); err != nil {
		return cmdErr(stderr, "inbox", diag.CodeStore, err)
	}
	return 0
}
