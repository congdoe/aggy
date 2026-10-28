package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/devfix"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/redact"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

const diagUsage = `Usage: aggy diag [filters] [--json | --jsonl] [--follow] [-n N]
       aggy diag show <id> [--json]
       aggy diag codes [code]
       aggy diag sql "<SELECT ...>" [--json]
       aggy diag export --jsonl [filters]
       aggy diag bundle [--trace ID] [--since WHEN]

Reads aggy's diagnostic log (~/.aggy/diag.db) and, for show, sql, and
bundle, the index too. Every form only reads: none creates, migrates, or
writes either file. docs/logging.md is the guide.

  show <id>   one timeline for a run, trace, borrow, child, parent, or
              record id: the diagnostic records, the index's events, and
              the borrow rows, in time order, indented by cause
  codes       every reason code (or one, or an event) with its meaning
              and what to check next
  sql         one SELECT on the records table (diag.records), with the
              index attached read-only as idx (idx.events, idx.borrows)
  export      the records as JSON lines, for jq or an agent
  bundle      one scrubbed Markdown file to hand to an agent or attach
              to an issue: the timeline, its codes, aggy doctor --report,
              versions, and config keys (never values)

Filters:
  --since WHEN, --until WHEN   1h, 30m, 2d, today, 2026-10-28, or RFC 3339
  --level L                    L and more severe: error, warn, info, debug
  --component C  --event E  --code C  --vendor V
                               --event and --code take globs (borrow.*)
  --run ID  --trace ID  --parent ID  --child ID  --borrow ID
  -n N                         the latest N records (default 100; 0: all)
  --follow                     keep printing new records until Ctrl-C
`

// diagPollEvery is how often --follow looks for new records.
var diagPollEvery = time.Second

func runDiag(args []string, stdout, stderr io.Writer) int {
	ctx, stop := execrun.NotifyContext(context.Background())
	defer stop()
	return diagWith(ctx, args, time.Now, stdout, stderr)
}

func diagWith(ctx context.Context, args []string, now func() time.Time, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	var err error
	switch sub {
	case "":
		err = diagList(ctx, args, now, false, stdout)
	case "export":
		err = diagList(ctx, args, now, true, stdout)
	case "show":
		err = diagShow(ctx, args, stdout)
	case "codes":
		err = diagCodes(args, stdout)
	case "sql":
		err = diagSQL(ctx, args, stdout)
	case "bundle":
		err = diagBundle(ctx, args, now, stdout)
	case "help":
		err = flag.ErrHelp
	default:
		err = usageError{fmt.Errorf("unknown subcommand %q", sub)}
	}
	var ue usageError
	switch {
	case errors.Is(err, flag.ErrHelp):
		fmt.Fprint(stdout, diagUsage)
		return 0
	case errors.As(err, &ue):
		// aggy diag only reads, so its own errors are not logged.
		fmt.Fprintf(stderr, "aggy diag: %s\n\n%s", termsafe.Field(err.Error()), diagUsage)
		return 2
	case err != nil:
		fmt.Fprintf(stderr, "aggy diag: %s\n", termsafe.Field(err.Error()))
		return 1
	}
	return 0
}

// usageError is a bad command line.
type usageError struct{ error }

func (e usageError) Unwrap() error { return e.error }

// openDiagView opens diag.db and the index read-only together.
func openDiagView(ctx context.Context) (*store.DiagView, error) {
	dp, err := store.DiagPath()
	if err != nil {
		return nil, err
	}
	ip, err := store.DefaultPath()
	if err != nil {
		return nil, err
	}
	return store.OpenDiagView(ctx, dp, ip)
}

// parseWhen reads a --since or --until value: a duration back from now
// (1h, 30m, 2d), today (local midnight), a date, or RFC 3339.
func parseWhen(s string, now time.Time) (time.Time, error) {
	if s == "today" {
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, now.Location()), nil
	}
	if n, ok := strings.CutSuffix(s, "d"); ok {
		if days, err := strconv.Atoi(n); err == nil && days >= 0 {
			return now.Add(-time.Duration(days) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, now.Location()); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not 1h, 2d, today, a date, or an RFC 3339 time", s)
}

// diagFlags are the filter flags shared by aggy diag and export.
type diagFlags struct {
	fs                         *flag.FlagSet
	since, until, level        *string
	follow, json, jsonl        *bool
	n                          *int
	component, event, code     *string
	vendor, run, trace, parent *string
	child, borrow              *string
}

func newDiagFlags(name string, n int) *diagFlags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return &diagFlags{
		fs: fs, since: fs.String("since", "", ""), until: fs.String("until", "", ""), level: fs.String("level", "", ""),
		follow: fs.Bool("follow", false, ""), json: fs.Bool("json", false, ""), jsonl: fs.Bool("jsonl", false, ""),
		n: fs.Int("n", n, ""), component: fs.String("component", "", ""), event: fs.String("event", "", ""),
		code: fs.String("code", "", ""), vendor: fs.String("vendor", "", ""), run: fs.String("run", "", ""),
		trace: fs.String("trace", "", ""), parent: fs.String("parent", "", ""), child: fs.String("child", "", ""),
		borrow: fs.String("borrow", "", ""),
	}
}

func (f *diagFlags) parse(args []string, now time.Time) (store.DiagFilter, error) {
	if err := f.fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return store.DiagFilter{}, err
		}
		return store.DiagFilter{}, usageError{err}
	}
	if f.fs.NArg() > 0 {
		return store.DiagFilter{}, usageError{fmt.Errorf("unexpected %s", f.fs.Arg(0))}
	}
	flt := store.DiagFilter{
		Component: *f.component, Event: *f.event, Code: *f.code, Vendor: *f.vendor, Run: *f.run,
		Trace: *f.trace, Parent: *f.parent, Child: *f.child, Borrow: *f.borrow, Limit: *f.n,
	}
	var err error
	if *f.since != "" {
		if flt.Since, err = parseWhen(*f.since, now); err != nil {
			return flt, usageError{fmt.Errorf("--since: %w", err)}
		}
	}
	if *f.until != "" {
		if flt.Until, err = parseWhen(*f.until, now); err != nil {
			return flt, usageError{fmt.Errorf("--until: %w", err)}
		}
	}
	if *f.level != "" {
		ls := diag.AtLeast(diag.Level(*f.level))
		if ls == nil {
			return flt, usageError{fmt.Errorf("--level %q: want error, warn, info, or debug", *f.level)}
		}
		for _, l := range ls {
			flt.Levels = append(flt.Levels, string(l))
		}
	}
	if *f.n < 0 {
		return flt, usageError{errors.New("-n must be 0 or more")}
	}
	if *f.json && *f.jsonl {
		return flt, usageError{errors.New("--json and --jsonl are one or the other")}
	}
	return flt, nil
}

// diagList is aggy diag (a table, --json, or --jsonl, and --follow) and
// aggy diag export (always JSON lines, every record unless -n).
func diagList(ctx context.Context, args []string, now func() time.Time, export bool, stdout io.Writer) error {
	f := newDiagFlags("diag", 100)
	if export {
		f = newDiagFlags("diag export", 0)
	}
	flt, err := f.parse(args, now())
	if err != nil {
		return err
	}
	if export && (*f.json || *f.follow) {
		return usageError{errors.New("export prints JSON lines only, once")}
	}
	if *f.follow && *f.json {
		return usageError{errors.New("--follow prints a table or --jsonl")}
	}
	v, err := openDiagView(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = v.Close() }()
	recs, err := v.Records(ctx, flt)
	if err != nil {
		return err
	}
	switch {
	case export || *f.jsonl:
		err = writeJSONL(stdout, recs)
	case *f.json:
		if recs == nil {
			recs = []store.DiagRecord{}
		}
		err = writeJSON(stdout, recs)
	case len(recs) == 0 && !*f.follow:
		if !v.HasDiag() {
			fmt.Fprintln(stdout, "No diagnostic records yet: aggy has not written ~/.aggy/diag.db.")
		} else {
			fmt.Fprintln(stdout, "No records match.")
		}
	default:
		printRecords(stdout, recs)
	}
	if err != nil || !*f.follow {
		return err
	}
	var last int64
	if len(recs) > 0 {
		last = recs[len(recs)-1].Row
	}
	flt.Limit = 0
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(diagPollEvery):
		}
		if !v.HasDiag() {
			// diag.db did not exist when the view opened; look again.
			_ = v.Close()
			if v, err = openDiagView(ctx); err != nil {
				return err
			}
		}
		flt.AfterRow = last
		recs, err := v.Records(ctx, flt)
		if err != nil {
			return err
		}
		if len(recs) == 0 {
			continue
		}
		last = recs[len(recs)-1].Row
		if *f.jsonl {
			err = writeJSONL(stdout, recs)
		} else {
			printRecords(stdout, recs)
		}
		if err != nil {
			return err
		}
	}
}

func writeJSONL(w io.Writer, recs []store.DiagRecord) error {
	enc := json.NewEncoder(w)
	for _, r := range recs {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// printRecords prints records as a table. Every field is escaped.
func printRecords(w io.Writer, recs []store.DiagRecord) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME (UTC)\tLEVEL\tEVENT\tCODE\tBORROW\tRUN\tMSG")
	for _, r := range recs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.TS.UTC().Format("2006-01-02 15:04:05.000"),
			termsafe.Field(r.Level), termsafe.Field(r.Event), termsafe.Field(dash(r.Code)), termsafe.Field(dash(r.BorrowID)),
			termsafe.Field(r.RunID), termsafe.Field(dash(r.Msg)))
	}
	_ = tw.Flush()
}

// timelineEntry is one line of aggy diag show: a diagnostic record, an
// index event, or a borrow row's start or end.
type timelineEntry struct {
	At     time.Time `json:"at"`
	Source string    `json:"source"` // diag, event, or borrow
	ID     string    `json:"id"`
	Cause  string    `json:"cause_id,omitempty"`
	Depth  int       `json:"depth"`
	Text   string    `json:"text"`
}

// idKinds says what id names among recs and the index.
func idKinds(id string, recs []store.DiagRecord, indexKind string) []string {
	seen := map[string]bool{}
	for _, r := range recs {
		for kind, v := range map[string]string{"record": r.ID, "run": r.RunID, "trace": r.TraceID, "borrow": r.BorrowID,
			"child": r.ChildID, "parent": r.ParentID, "worktree": r.WorktreeID} {
			if v == id {
				seen[kind] = true
			}
		}
	}
	if indexKind != "" {
		seen[indexKind] = true
	}
	var out []string
	for _, k := range []string{"record", "run", "trace", "borrow", "child", "parent", "worktree"} {
		if seen[k] {
			out = append(out, k)
		}
	}
	return out
}

// timeline builds aggy diag show's entries for id, and what id names.
func timeline(ctx context.Context, v *store.DiagView, id string) ([]timelineEntry, []string, error) {
	recs, err := v.RecordsAbout(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	ik, err := v.IndexKind(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	kinds := idKinds(id, recs, ik)
	if len(kinds) == 1 && kinds[0] == "record" && recs[0].TraceID != "" {
		// One record: show the flow it is part of.
		if recs, err = v.RecordsAbout(ctx, recs[0].TraceID); err != nil {
			return nil, nil, err
		}
	}
	add := func(set map[string]bool, list *[]string, s string) {
		if s != "" && !set[s] {
			set[s] = true
			*list = append(*list, s)
		}
	}
	seen := map[string]bool{}
	var borrows, children, parents, runs []string
	for _, k := range kinds {
		switch k {
		case "borrow":
			add(seen, &borrows, id)
		case "child":
			add(seen, &children, id)
		case "parent":
			add(seen, &parents, id)
		case "run":
			add(seen, &runs, id)
		}
	}
	for _, r := range recs {
		add(seen, &borrows, r.BorrowID)
		add(seen, &children, r.ChildID)
		add(seen, &runs, r.RunID)
	}
	rows, err := v.BorrowRows(ctx, borrows, children)
	if err != nil {
		return nil, nil, err
	}
	for _, b := range rows {
		add(seen, &children, b.ChildID)
	}
	events, err := v.EventsAbout(ctx, children, parents, borrows, runs)
	if err != nil {
		return nil, nil, err
	}

	var out []timelineEntry
	byID := map[string]store.DiagRecord{}
	for _, r := range recs {
		byID[r.ID] = r
	}
	for _, r := range recs {
		depth := 0
		for c, n := r.CauseID, 0; c != "" && n < 32; n++ {
			p, ok := byID[c]
			if !ok {
				break
			}
			depth++
			c = p.CauseID
		}
		out = append(out, timelineEntry{At: r.TS, Source: "diag", ID: r.ID, Cause: r.CauseID, Depth: depth, Text: diagLine(r)})
	}
	for _, e := range events {
		out = append(out, timelineEntry{At: e.At, Source: "event", ID: strconv.FormatInt(e.ID, 10),
			Text: termsafe.Field(e.Type + ": " + describe(e))})
	}
	for _, b := range rows {
		out = append(out, timelineEntry{At: b.StartedAt, Source: "borrow", ID: b.ID,
			Text: termsafe.Field(fmt.Sprintf("borrow %s started: %s on %s (child %s)", b.ID, b.Kind, withModel(b.Vendor, b.Model), dash(b.ChildID)))})
		if !b.EndedAt.IsZero() {
			out = append(out, timelineEntry{At: b.EndedAt, Source: "borrow", ID: b.ID,
				Text: termsafe.Field(fmt.Sprintf("borrow %s %s: %s, %d tokens", b.ID, b.Status, dash(b.Outcome), b.Tokens))})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, kinds, nil
}

// diagLine is one record on one line, escaped. The vendor session id
// is left out: it is the vendor's, and a timeline may be pasted.
func diagLine(r store.DiagRecord) string {
	parts := []string{r.Level, r.Event}
	if r.Code != "" {
		parts = append(parts, "["+r.Code+"]")
	}
	for _, kv := range [][2]string{{"run", r.RunID}, {"borrow", r.BorrowID}, {"child", r.ChildID}, {"vendor", r.Vendor}} {
		if kv[1] != "" {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	if r.ExitCode != nil {
		parts = append(parts, fmt.Sprintf("exit=%d", *r.ExitCode))
	}
	if r.DurationMS != nil {
		parts = append(parts, (time.Duration(*r.DurationMS) * time.Millisecond).String())
	}
	if len(r.Attrs) > 0 {
		parts = append(parts, string(r.Attrs))
	}
	if r.Msg != "" {
		parts = append(parts, "- "+r.Msg)
	}
	return termsafe.Field(strings.Join(parts, " "))
}

func diagShow(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("diag show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "")
	var id string
	// The id may come before or after --json.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if id == "" && fs.NArg() == 1 {
		id = fs.Arg(0)
	} else if fs.NArg() > 0 || id == "" {
		return usageError{errors.New("show takes one id")}
	}
	v, err := openDiagView(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = v.Close() }()
	entries, kinds, err := timeline(ctx, v, id)
	if err != nil {
		return err
	}
	if len(kinds) == 0 {
		return fmt.Errorf("no run, trace, borrow, child, parent, or record has the id %q", id)
	}
	if *asJSON {
		return writeJSON(stdout, map[string]any{"id": id, "kinds": kinds, "timeline": entries})
	}
	fmt.Fprintf(stdout, "%s is a %s id\n", termsafe.Field(id), strings.Join(kinds, " and "))
	for _, e := range entries {
		fmt.Fprintf(stdout, "%s  %-6s %s%s\n", e.At.UTC().Format("2006-01-02 15:04:05.000"), e.Source, strings.Repeat("  ", e.Depth), e.Text)
	}
	return nil
}

func diagCodes(args []string, stdout io.Writer) error {
	if len(args) > 1 || (len(args) == 1 && strings.HasPrefix(args[0], "-")) {
		if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
			return flag.ErrHelp
		}
		return usageError{errors.New("codes takes at most one code")}
	}
	if len(args) == 1 {
		if c, ok := diag.LookupCode(diag.Code(args[0])); ok {
			printCode(stdout, c)
			return nil
		}
		if e, ok := diag.LookupEvent(diag.Event(args[0])); ok {
			fmt.Fprintf(stdout, "%s (event, %s, component %s)\n  %s\n  next: %s\n", e.Name, e.Level, e.Component, e.Meaning, e.Next)
			return nil
		}
		return fmt.Errorf("no code or event %q; aggy diag codes lists them", args[0])
	}
	for i, c := range diag.Codes() {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		printCode(stdout, c)
	}
	return nil
}

func printCode(w io.Writer, c diag.CodeInfo) {
	sec := ""
	if c.Security {
		sec = " (security: also a refusal in aggy log)"
	}
	fmt.Fprintf(w, "%s%s\n  %s\n  next: %s\n", c.Name, sec, c.Meaning, c.Next)
}

func diagSQL(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("diag sql", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "")
	var q string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		q, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if q == "" && fs.NArg() == 1 {
		q = fs.Arg(0)
	} else if fs.NArg() > 0 || strings.TrimSpace(q) == "" {
		return usageError{errors.New(`sql takes one quoted SELECT`)}
	}
	v, err := openDiagView(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = v.Close() }()
	cols, rows, err := v.Query(ctx, q)
	if err != nil {
		return err
	}
	cell := func(x any) any {
		if b, ok := x.([]byte); ok {
			return string(b)
		}
		return x
	}
	if *asJSON {
		out := make([]map[string]any, 0, len(rows))
		for _, r := range rows {
			m := map[string]any{}
			for i, c := range cols {
				m[c] = cell(r[i])
			}
			out = append(out, m)
		}
		return writeJSON(stdout, out)
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	esc := make([]string, len(cols))
	for i, c := range cols {
		esc[i] = termsafe.Field(c)
	}
	fmt.Fprintln(tw, strings.Join(esc, "\t"))
	for _, r := range rows {
		for i, x := range r {
			if x == nil {
				esc[i] = "NULL"
			} else {
				esc[i] = termsafe.Field(fmt.Sprint(cell(x)))
			}
		}
		fmt.Fprintln(tw, strings.Join(esc, "\t"))
	}
	return tw.Flush()
}

// configKeys lists the keys of the TOML file at path, never its values:
// "" when it does not exist.
func configKeys(path string) (string, error) {
	b, err := safefs.ReadConfig(path, config.MaxFileSize)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var m map[string]any
	if _, err := toml.Decode(string(b), &m); err != nil {
		return "", errors.New("it does not parse")
	}
	var keys []string
	var walk func(prefix string, m map[string]any)
	walk = func(prefix string, m map[string]any) {
		for k, v := range m {
			if sub, ok := v.(map[string]any); ok {
				walk(prefix+k+".", sub)
				continue
			}
			keys = append(keys, prefix+k)
		}
	}
	walk("", m)
	sort.Strings(keys)
	return strings.Join(keys, ", "), nil
}

// bundleRecords caps the timeline a bundle carries.
const bundleRecords = 500

func diagBundle(ctx context.Context, args []string, now func() time.Time, stdout io.Writer) error {
	fs := flag.NewFlagSet("diag bundle", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	trace := fs.String("trace", "", "")
	since := fs.String("since", "24h", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected %s", fs.Arg(0))}
	}
	flt := store.DiagFilter{Trace: *trace, Limit: bundleRecords}
	var err error
	if flt.Since, err = parseWhen(*since, now()); err != nil {
		return usageError{fmt.Errorf("--since: %w", err)}
	}
	v, err := openDiagView(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = v.Close() }()
	recs, err := v.Records(ctx, flt)
	if err != nil {
		return err
	}
	dv, err := v.DiagVersion(ctx)
	if err != nil {
		return err
	}

	id := devfix.CurrentIdentity()
	sc := devfix.NewScrubber(id)
	// Everything in the bundle but aggy's own registry text goes through
	// this: the doctor report's scrub, the fixture scrubber, and redact.
	clean := func(s string) string {
		s, _ = redact.Lines(sc.String(reportScrub(s, id)))
		return s
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# aggy diagnostic bundle\n\nMade %s by `aggy diag bundle`. Home paths, user and host names, emails, and token-shaped text are scrubbed; it holds no environment value, file content, task text, or vendor session id.\n\n",
		now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "## Versions\n\n- aggy: %s\n- os: %s/%s\n- go: %s\n- diag.db schema: %d\n- index schema: %d\n\n",
		clean(version()), runtime.GOOS, runtime.GOARCH, runtime.Version(), dv, v.IndexVersion())
	sel := "since " + flt.Since.UTC().Format(time.RFC3339)
	if *trace != "" {
		sel = "trace " + *trace + ", " + sel
	}
	fmt.Fprintf(&b, "## Timeline\n\n%d record(s), %s (at most %d, the latest).\n\n```text\n", len(recs), clean(termsafe.Field(sel)), bundleRecords)
	seen := map[string]bool{}
	var seenCodes []string
	for _, r := range recs {
		fmt.Fprintf(&b, "%s %s\n", r.TS.UTC().Format("2006-01-02T15:04:05.000Z"), clean(diagLine(r)))
		if r.Code != "" && !seen[r.Code] {
			seen[r.Code] = true
			seenCodes = append(seenCodes, r.Code)
		}
	}
	b.WriteString("```\n\n## Codes\n\n")
	if len(seenCodes) == 0 {
		b.WriteString("None in this timeline.\n")
	}
	for _, c := range seenCodes {
		if info, ok := diag.LookupCode(diag.Code(c)); ok {
			fmt.Fprintf(&b, "- `%s`: %s Next: %s\n", info.Name, info.Meaning, info.Next)
		}
	}
	var rep strings.Builder
	printReport(&rep, doctor(ctx, defaultCheckEnv()), id)
	fmt.Fprintf(&b, "\n## aggy doctor --report\n\n```text\n%s```\n\n## Config keys (never values)\n\n", rep.String())
	var files [][2]string
	if p, err := config.Path(); err == nil {
		files = append(files, [2]string{"user config", p})
	}
	if cwd, err := os.Getwd(); err == nil {
		if root, ok := repoRoot(cwd); ok {
			files = append(files, [2]string{"repo config", config.RepoPath(root)})
		}
	}
	for _, f := range files {
		keys, err := configKeys(f[1])
		switch {
		case err != nil:
			keys = "unreadable: " + err.Error()
		case keys == "":
			keys = "none (no file, or empty)"
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", f[0], clean(f[1]), clean(termsafe.Field(keys)))
	}
	_, err = io.WriteString(stdout, b.String())
	return err
}
