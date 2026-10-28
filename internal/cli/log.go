package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const logUsage = `Usage: aggy log [-n N] [--parent ID]

Shows what happened, oldest first: which harness and model were on
screen, switches, borrows, and files changed.

  -n N          show only the latest N events
  --parent ID   show only this aggy session's events
`

// logSchema is the oldest schema with the events table.
const logSchema = 1

// runLog is aggy log. It never creates the store: with no database there
// is nothing to show.
func runLog(args []string, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("log", flag.ContinueOnError)
	fl.SetOutput(io.Discard)
	n := fl.Int("n", 0, "")
	parent := fl.String("parent", "", "")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, logUsage)
			return 0
		}
		fmt.Fprintf(stderr, "aggy log: %v\n\n%s", err, logUsage)
		return 2
	}
	if fl.NArg() > 0 || *n < 0 {
		fmt.Fprintf(stderr, "aggy log: unexpected arguments\n\n%s", logUsage)
		return 2
	}
	path, err := store.DefaultPath()
	if err != nil {
		fmt.Fprintf(stderr, "aggy log: %v\n", err)
		return 1
	}
	if _, err := safefs.Stat(path); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintln(stdout, "No events yet.")
		return 0
	} else if err != nil {
		fmt.Fprintf(stderr, "aggy log: %v\n", err)
		return 1
	}
	// Read only: aggy log never creates, migrates, or writes the index.
	ctx := context.Background()
	s, err := store.OpenReadOnly(ctx, path)
	if err != nil {
		fmt.Fprintf(stderr, "aggy log: %v\n", err)
		return 1
	}
	defer func() { _ = s.Close() }()
	if v, err := s.Version(ctx); err != nil {
		fmt.Fprintf(stderr, "aggy log: %v\n", err)
		return 1
	} else if v < logSchema {
		fmt.Fprintf(stdout, "The index is at schema version %d and has no event log yet. Run another aggy command to upgrade it.\n", v)
		return 0
	}
	events, err := s.Events(ctx, store.EventFilter{ParentID: *parent, Limit: *n})
	if err != nil {
		fmt.Fprintf(stderr, "aggy log: %v\n", store.ReadError(err))
		return 1
	}
	if len(events) == 0 {
		fmt.Fprintln(stdout, "No events yet.")
		return 0
	}
	// Every field is escaped, tab included: the events table holds text
	// from vendor sessions, and a tab would also break the columns.
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME (UTC)\tSESSION\tEVENT\tDETAIL")
	for _, e := range events {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", e.At.UTC().Format("2006-01-02 15:04:05"),
			termsafe.Field(dash(e.ParentID)), termsafe.Field(e.Type), termsafe.Field(describe(e)))
	}
	_ = tw.Flush()
	return 0
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// str returns detail[k] as a string, or "".
func str(d map[string]any, k string) string {
	if v, ok := d[k]; ok && v != nil {
		return fmt.Sprint(v)
	}
	return ""
}

func withModel(vendor, model string) string {
	if model == "" {
		return vendor
	}
	return vendor + " (" + model + ")"
}

// describe renders one event's detail on one line.
func describe(e store.Event) string {
	d := e.Detail
	switch e.Type {
	case store.EventScreen:
		return withModel(str(d, "vendor"), str(d, "model")) + " on screen"
	case store.EventSwitch:
		s := str(d, "from") + " -> " + withModel(str(d, "to"), str(d, "model"))
		if r := str(d, "reason"); r != "" {
			s += " [" + r + "]"
		}
		return s
	case store.EventBorrow:
		s := str(d, "kind") + " on " + withModel(str(d, "vendor"), str(d, "model"))
		if o := str(d, "outcome"); o != "" {
			s += ": " + o
		}
		return s
	case store.EventFiles:
		if fs, ok := d["files"].([]any); ok {
			names := make([]string, len(fs))
			for i, f := range fs {
				names[i] = fmt.Sprint(f)
			}
			return strings.Join(names, ", ")
		}
	}
	// Unknown types show their detail as sorted key=value pairs.
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + str(d, k)
	}
	return strings.Join(parts, " ")
}
