package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/store"
)

func runDiagCmd(args ...string) (int, string, string) {
	var out, errb strings.Builder
	code := diagWith(context.Background(), args, time.Now, &out, &errb)
	return code, out.String(), errb.String()
}

// allDiagText is every column of every row of diag.db, as text.
func allDiagText(t *testing.T, state string) string {
	t.Helper()
	v, err := store.OpenDiagView(context.Background(), filepath.Join(state, store.DiagFileName), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v.Close() }()
	var rows [][]any
	_, err = v.Query(context.Background(), "SELECT * FROM diag.records", 0,
		func([]string) error { return nil },
		func(r []any) error { rows = append(rows, r); return nil })
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range rows {
		for _, x := range r {
			if bs, ok := x.([]byte); ok {
				x = string(bs)
			}
			fmt.Fprintf(&b, "%v\t", x)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// TestDiagPrivacyCanary (T6753): a full fake edit borrow, accept, and gc,
// with a planted secret in the task, a constraint, the child's reply, and
// the edited file, and a distinctive task, leaves neither in any column
// of diag.db, in the export, or in the bundle. The records it needs are
// there, linked by borrow and trace, and the child got the trace. Each
// step runs with its own run id and trace, as its own process would, so
// the link comes from the borrow's row.
func TestDiagPrivacyCanary(t *testing.T) {
	f, markers := editFixture(t)
	gitIn(t, f.repo, "checkout", "--", "a.go")
	const secret = "ghp_Zq8uLw3RkT9vXy2NbM5cJd7HsA1pQe4Fg6Ui"
	const task = "Rewrite a.go so the periwinkle-otter-canary function returns forty-two"
	const word = "periwinkle-otter-canary"
	if err := os.WriteFile(filepath.Join(f.state, "config.toml"), []byte("[log]\nretention_days = 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sc, _ := json.Marshal(map[string]any{
		"events": []any{map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message",
			"text": "done: " + task + " token=" + secret}}},
		"cwd_files": []map[string]string{{"path": "a.go", "content": "package a\n\n// " + secret + "\nfunc Edited() {}\n"}},
	})
	f.scenario["codex"] = string(sc)
	freshRun(t)
	if code, out, errOut := f.run(t, "edit", task+" with key "+secret, "--vendor", "codex", "--constraint", "never print "+secret); code != 0 {
		t.Fatalf("borrow: code %d: %s %s", code, out, errOut)
	}
	var id string
	for _, r := range fakecli.Records(t, f.record) {
		if r.Mode == "headless" {
			id = filepath.Base(r.Cwd)
			if r.Env[diag.TraceEnv] != dlog.Trace() {
				t.Errorf("the child's AGGY_TRACE = %q, want %q", r.Env[diag.TraceEnv], dlog.Trace())
			}
		}
	}
	borrowTrace := dlog.Trace()
	freshRun(t)
	if code, out, errOut := accept(f, "", false, id, "--yes"); code != 0 {
		t.Fatalf("accept: code %d: %s %s", code, out, errOut)
	}
	freshRun(t)
	later := time.Now().Add(40 * 24 * time.Hour)
	f.env.now = func() time.Time { return later }
	if code, out, errOut := gc(f, "--yes"); code != 0 || !strings.Contains(out, "Removed worktree") {
		t.Fatalf("gc: code %d: %s %s", code, out, errOut)
	}
	noMarkers(t, markers)

	text := allDiagText(t, f.state)
	for _, ev := range []diag.Event{diag.EvBorrowChildStarted, diag.EvBorrowChildExited, diag.EvBorrowFinished, diag.EvAcceptApplied, diag.EvGCRemoved} {
		if !strings.Contains(text, ev.String()) {
			t.Errorf("no %s record", ev)
		}
	}
	_, exp, _ := runDiagCmd("export", "--jsonl")
	_, bundle, errB := runDiagCmd("bundle", "--since", "1h")
	if !strings.Contains(exp, id) || !strings.Contains(bundle, diag.EvAcceptApplied.String()) {
		t.Errorf("export or bundle misses the borrow: %s\n%s %s", exp, bundle, errB)
	}
	for name, s := range map[string]string{"diag.db": text, "export": exp, "bundle": bundle} {
		for _, leak := range []string{secret, secret[:16], task, word, "forty-two"} {
			if strings.Contains(s, leak) {
				t.Errorf("%s holds %q", name, leak)
			}
		}
	}
	// Linked: the accept's record about the borrow has its trace.
	recs := exportRecords(t, exp)
	traces := map[string]bool{}
	for _, r := range recs {
		if r.BorrowID == id {
			traces[r.TraceID] = true
		}
	}
	if len(traces) != 1 || !traces[borrowTrace] {
		t.Errorf("the borrow's records have traces %v, want %s alone", traces, borrowTrace)
	}
}

func exportRecords(t *testing.T, jsonl string) []store.DiagRecord {
	t.Helper()
	var out []store.DiagRecord
	for _, l := range strings.Split(strings.TrimSpace(jsonl), "\n") {
		if l == "" {
			continue
		}
		var r store.DiagRecord
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		out = append(out, r)
	}
	return out
}

// TestDiagCommands: the query forms over a borrow's records.
func TestDiagCommands(t *testing.T) {
	f := newBorrowFixture(t, adapter.Grok)
	f.scenario["grok"] = `{"json":{"result":"an answer"}}`
	code, out, errOut := f.run(t, "research", "what is new in go", "--vendor", "grok", "--fresh")
	if code != 0 {
		t.Fatalf("borrow: code %d: %s %s", code, out, errOut)
	}
	borrowID := strings.Fields(out)[1]
	borrowID = strings.TrimSuffix(borrowID, ":")

	if code, out, _ := runDiagCmd("--borrow", borrowID); code != 0 || !strings.Contains(out, "borrow.child.started") || !strings.Contains(out, "TIME (UTC)") {
		t.Errorf("table: %d %s", code, out)
	}
	if code, out, _ := runDiagCmd("--borrow", borrowID, "--json"); code != 0 || !strings.HasPrefix(out, "[") {
		t.Errorf("--json: %d %s", code, out)
	}
	code, out, _ = runDiagCmd("--borrow", borrowID, "--jsonl", "--event", "borrow.child.*")
	if recs := exportRecords(t, out); code != 0 || len(recs) != 2 {
		t.Errorf("--jsonl --event glob: %d %s", code, out)
	}
	if code, out, _ := runDiagCmd("--borrow", borrowID, "--level", "error"); code != 0 || !strings.Contains(out, "No records match") {
		t.Errorf("--level error: %d %s", code, out)
	}
	code, out, errOut = runDiagCmd("show", borrowID)
	if code != 0 || !strings.Contains(out, "is a borrow id") || !strings.Contains(out, "borrow.run.finished") ||
		!strings.Contains(out, "event  borrow:") || !strings.Contains(out, "started: research on grok") {
		t.Errorf("show borrow: %d %s %s", code, out, errOut)
	}
	// The exit is caused by the start: indented under it.
	if !strings.Contains(out, "diag     info borrow.child.exited") {
		t.Errorf("show does not indent by cause: %s", out)
	}
	if code, out, _ := runDiagCmd("show", dlog.RunID(), "--json"); code != 0 || !strings.Contains(out, `"run"`) {
		t.Errorf("show run: %d %s", code, out)
	}
	if code, _, errOut := runDiagCmd("show", "nope"); code != 1 || !strings.Contains(errOut, "no run, trace") {
		t.Errorf("show unknown: %d %s", code, errOut)
	}
	if code, out, _ := runDiagCmd("codes"); code != 0 || !strings.Contains(out, "lockdown.rejected (security") {
		t.Errorf("codes: %d %s", code, out)
	}
	if code, out, _ := runDiagCmd("codes", "borrow.one_hop"); code != 0 || !strings.Contains(out, "next:") {
		t.Errorf("codes one: %d %s", code, out)
	}
	if code, out, _ := runDiagCmd("codes", "borrow.child.exited"); code != 0 || !strings.Contains(out, "event") {
		t.Errorf("codes event: %d %s", code, out)
	}
	if code, _, _ := runDiagCmd("codes", "nope.nope"); code != 1 {
		t.Errorf("codes unknown: %d", code)
	}
	q := "SELECT r.event, b.status FROM records r JOIN idx.borrows b ON b.id = r.borrow_id WHERE r.event = 'borrow.run.finished' AND r.borrow_id = '" + borrowID + "'"
	if code, out, errOut := runDiagCmd("sql", q); code != 0 || !strings.Contains(out, "borrow.run.finished  done") {
		t.Errorf("sql: %d %s %s", code, out, errOut)
	}
	if code, out, _ := runDiagCmd("sql", "SELECT count(*) AS n FROM events", "--json"); code != 0 || !strings.Contains(out, `"n"`) {
		t.Errorf("sql --json: %d %s", code, out)
	}
	for _, w := range []string{"DELETE FROM records", "UPDATE idx.borrows SET status = 'x'", "ATTACH DATABASE 'x.db' AS x", "PRAGMA query_only = 0"} {
		if code, _, _ := runDiagCmd("sql", w); code != 1 {
			t.Errorf("sql %q: code %d", w, code)
		}
	}
	for _, bad := range [][]string{{"--since", "yesterday-ish"}, {"--level", "loud"}, {"nope"}, {"show"}, {"sql"}, {"--json", "--jsonl"}, {"export", "--follow"}} {
		if code, _, _ := runDiagCmd(bad...); code != 2 {
			t.Errorf("%v: code %d, want 2", bad, code)
		}
	}
	if code, out, _ := runDiagCmd("--help"); code != 0 || !strings.Contains(out, "aggy diag show") {
		t.Errorf("--help: %d %s", code, out)
	}
}

// TestDiagFollow: --follow prints records written after it started, and
// ends with its context.
func TestDiagFollow(t *testing.T) {
	newBorrowFixture(t)
	old := diagPollEvery
	diagPollEvery = 10 * time.Millisecond
	t.Cleanup(func() { diagPollEvery = old })
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuilder
	done := make(chan int)
	go func() { done <- diagWith(ctx, []string{"--follow", "--jsonl"}, time.Now, &out, &out) }()
	time.Sleep(50 * time.Millisecond)
	dlog.Log(context.Background(), diag.Rec{Event: diag.EvGCRemoved, Msg: "followed"})
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "followed") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 || !strings.Contains(out.String(), "followed") {
		t.Errorf("follow: code %d: %s", code, out.String())
	}
}

// TestDiagReadOnly: no aggy diag form creates diag.db or the index.
func TestDiagReadOnly(t *testing.T) {
	state := t.TempDir()
	t.Setenv("AGGY_HOME", state)
	for _, args := range [][]string{{}, {"--json"}, {"show", "x"}, {"codes"}, {"sql", "SELECT 1"}, {"export", "--jsonl"}, {"bundle"}} {
		runDiagCmd(args...)
	}
	if ents, _ := os.ReadDir(state); len(ents) != 0 {
		t.Errorf("aggy diag made files: %v", ents)
	}
	if code, out, _ := runDiagCmd(); code != 0 || !strings.Contains(out, "No diagnostic records yet") {
		t.Errorf("empty: %d %s", code, out)
	}
}

// TestRefusalTagged: a refusal prints its code and run id, is in
// diag.db, and, being a security refusal, is a refusal row in aggy log.
func TestRefusalTagged(t *testing.T) {
	f := newBorrowFixture(t)
	f.env.getenv = func(k string) string { return map[string]string{"AGGY_DELEGATED": "1"}[k] }
	code, _, errOut := f.run(t, "research", "x")
	tag := "[borrow.one_hop · run " + dlog.RunID() + "]"
	if code != 1 || !strings.HasSuffix(strings.TrimSpace(errOut), tag) {
		t.Fatalf("code %d: %q", code, errOut)
	}
	if !strings.Contains(allDiagText(t, f.state), diag.CodeOneHop.String()) {
		t.Error("no record")
	}
	var out, errb strings.Builder
	if code := runLog(nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "refusal") || !strings.Contains(out.String(), "borrow.one_hop") {
		t.Errorf("aggy log: %d %s %s", code, out.String(), errb.String())
	}
}

// TestGCPrunesDiag: aggy gc deletes records past [log] retention_days;
// --dry-run counts them and deletes nothing.
func TestGCPrunesDiag(t *testing.T) {
	f := newBorrowFixture(t)
	ctx := context.Background()
	d, err := store.OpenDiag(ctx, filepath.Join(f.state, store.DiagFileName))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for i, age := range []time.Duration{30 * 24 * time.Hour, 20 * 24 * time.Hour, time.Hour} {
		r := store.DiagRecord{ID: fmt.Sprintf("old%d", i), Schema: 1, TS: now.Add(-age), Level: "info", Component: "gc", Event: diag.EvGCRemoved.String(), RunID: "r"}
		if err := d.Insert(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	_ = d.Close()
	if code, out, _ := gc(f, "--dry-run"); code != 0 || !strings.Contains(out, "2 diagnostic record(s) older than 14 days") {
		t.Errorf("dry run: %d %s", code, out)
	}
	if code, out, errOut := gc(f, "--yes"); code != 0 || !strings.Contains(out, "Removed diag") {
		t.Fatalf("gc: %d %s %s", code, out, errOut)
	}
	text := allDiagText(t, f.state)
	if strings.Contains(text, "old0") || strings.Contains(text, "old1") || !strings.Contains(text, "old2") {
		t.Errorf("after gc: %s", text)
	}
}

// syncBuilder is a strings.Builder safe for one writer and one reader.
type syncBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestParseWhen(t *testing.T) {
	now := time.Date(2026, 10, 28, 15, 30, 0, 0, time.UTC)
	for _, tt := range []struct {
		in   string
		want time.Time
	}{
		{"1h", now.Add(-time.Hour)},
		{"2d", now.Add(-48 * time.Hour)},
		{"today", time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)},
		{"2026-10-01", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)},
		{"2026-10-01T10:00:00Z", time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)},
	} {
		if got, err := parseWhen(tt.in, now); err != nil || !got.Equal(tt.want) {
			t.Errorf("%s = %v, %v; want %v", tt.in, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "-1h", "soon", "-2d"} {
		if _, err := parseWhen(bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// freshRun gives the next command a writer of its own, as a new aggy
// process has: a new run id, and a new trace unless AGGY_TRACE is set.
// The old writer is back after the test.
func freshRun(t *testing.T) {
	t.Helper()
	old := dlog
	t.Cleanup(func() { dlog = old })
	dlog = diag.New(version(), os.Stderr)
}

// TestDiagPrivacyCanaryFailures (T6753, P58-5): the failure paths store
// none of a planted secret, a distinctive task, a check's output, or an
// env value: a failed borrow, one that never started, a failed check,
// and an accept refusal, whose security code also writes a refusal row
// to the index's events. Each step runs as its own process would.
func TestDiagPrivacyCanaryFailures(t *testing.T) {
	const secret = "ghp_Rf4Kw9Tz2Lq7Xm3Bv8Nc1Hd6Js5Pa0Ye2Gu"
	const word = "tangerine-heron-canary"
	const task = "Make b.go print the " + word + " banner"
	const envValue = "plum-envvalue-canary-55913"
	const checkOut = "marmalade-checkoutput-canary"
	t.Setenv("OPENAI_API_KEY", envValue)
	t.Setenv("AGGY_CANARY_ENV", envValue)
	f, id, _, _, _ := gateFixture(t, "echo "+checkOut+" >&2; echo "+secret+" >&2; exit 1")
	if err := os.WriteFile(filepath.Join(f.state, "config.toml"), []byte("[log]\nlevel = \"debug\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A failed check.
	freshRun(t)
	if code, out, _ := checkEdit(f, "", false, id, "--yes"); code != 1 || !strings.Contains(out, checkOut) {
		t.Fatalf("check: code %d: %s", code, out)
	}
	// A borrow whose child fails, with the secret in its task, its
	// constraint, its reply, and its stderr.
	freshRun(t)
	sc, _ := json.Marshal(map[string]any{
		"events":    []any{map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": task + " " + secret}}},
		"stderr":    "fatal: " + task + " " + secret + "\n",
		"exit_code": 3,
	})
	f.scenario["codex"] = string(sc)
	if code, out, errOut := f.run(t, "review", task+" key "+secret, "--vendor", "codex", "--constraint", "hide "+secret); code == 0 {
		t.Fatalf("failed borrow: code 0: %s %s", out, errOut)
	}
	// One that never starts: grok is not installed.
	freshRun(t)
	if code, out, errOut := f.run(t, "research", task+" key "+secret, "--vendor", "grok"); code == 0 {
		t.Fatalf("no-start borrow: code 0: %s %s", out, errOut)
	}
	// An accept refused for a protected path (a security code).
	freshRun(t)
	sc, _ = json.Marshal(map[string]any{
		"events": []any{map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": "done " + secret}}},
		"cwd_files": []map[string]string{
			{"path": "b.go", "content": "package a\n// " + word + " " + secret + "\n"},
			{"path": ".github/workflows/ci.yml", "content": "run: echo " + secret + "\n"},
		},
	})
	f.scenario["codex"] = string(sc)
	before := len(fakecli.Records(t, f.record))
	if code, out, errOut := f.run(t, "edit", task+" key "+secret, "--vendor", "codex", "--constraint", "hide "+secret); code != 0 {
		t.Fatalf("edit: code %d: %s %s", code, out, errOut)
	}
	var edit string
	for _, r := range fakecli.Records(t, f.record)[before:] {
		if r.Mode == "headless" {
			edit = filepath.Base(r.Cwd)
		}
	}
	freshRun(t)
	if code, _, errOut := accept(f, "", false, edit, "--yes"); code != 1 || !strings.Contains(errOut, "[accept.protected_path · run "+dlog.RunID()+"]") {
		t.Fatalf("accept: code %d: %s", code, errOut)
	}

	text := allDiagText(t, f.state)
	for _, want := range []string{diag.EvCheckCommandFailed.String(), diag.CodeChecksFailed.String(), diag.EvBorrowFailed.String(),
		diag.EvBorrowRefused.String(), diag.EvAcceptRefused.String(), diag.CodeProtected.String()} {
		if !strings.Contains(text, want) {
			t.Errorf("no %s in diag.db:\n%s", want, text)
		}
	}
	_, exp, _ := runDiagCmd("export", "--jsonl")
	_, bundle, _ := runDiagCmd("bundle", "--since", "1h")
	code, events, errOut := runDiagCmd("sql", "SELECT * FROM idx.events", "--json")
	if code != 0 || !strings.Contains(events, `"refusal"`) || !strings.Contains(events, diag.CodeProtected.String()) {
		t.Errorf("idx.events: code %d: %s %s", code, events, errOut)
	}
	for name, s := range map[string]string{"diag.db": text, "export": exp, "bundle": bundle, "idx.events": events} {
		for _, leak := range []string{secret, secret[:16], task, word, envValue, checkOut} {
			if strings.Contains(s, leak) {
				t.Errorf("%s holds %q", name, leak)
			}
		}
	}
}

// TestDiagNoStateNoFiles (P58-1): a refusal with no state dir, a dry run
// included, makes no file; a dry run where the state dir is there makes
// no diag.db and writes no index row.
func TestDiagNoStateNoFiles(t *testing.T) {
	f := newBorrowFixture(t)
	t.Setenv("PATH", t.TempDir())
	f.env.getenv = func(k string) string {
		if k == "AGGY_DELEGATED" {
			return "1"
		}
		return ""
	}
	parent := t.TempDir()
	t.Setenv("AGGY_HOME", filepath.Join(parent, "aggy"))
	for _, args := range [][]string{{"--dry-run"}, {"--yes"}} {
		freshRun(t)
		if code, _, errOut := gc(f, args...); code != 1 || !strings.Contains(errOut, "borrow child") && errOut == "" || strings.Contains(errOut, "warning") {
			t.Errorf("%v: code %d: %s", args, code, errOut)
		}
		if ents, err := os.ReadDir(parent); err != nil || len(ents) != 0 {
			t.Errorf("%v: made %v, %v", args, ents, err)
		}
	}
	// The state dir and the index exist: a dry run's refusal stores nothing.
	t.Setenv("AGGY_HOME", f.state)
	freshRun(t)
	if code, out, errOut := gc(f, "--dry-run"); code != 1 || strings.Contains(errOut, "[") {
		t.Errorf("dry run: code %d: %s %s", code, out, errOut)
	}
	if _, err := os.Stat(filepath.Join(f.state, store.DiagFileName)); !os.IsNotExist(err) {
		t.Errorf("diag.db: %v", err)
	}
	s, err := store.OpenReadOnly(context.Background(), filepath.Join(f.state, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if evs, err := s.Events(context.Background(), store.EventFilter{}); err != nil || len(evs) != 0 {
		t.Errorf("events = %+v, %v", evs, err)
	}
}

// TestGCCorruptDiag (P58-2): a diag.db that cannot be read is warned
// about once, by name, and gc goes on to exit 0.
func TestGCCorruptDiag(t *testing.T) {
	f := newBorrowFixture(t)
	t.Setenv("PATH", t.TempDir())
	if err := os.WriteFile(filepath.Join(f.state, store.DiagFileName), []byte(strings.Repeat("not a database ", 400)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--dry-run"}, {"--yes"}} {
		freshRun(t)
		code, out, errOut := gc(f, args...)
		if code != 0 || strings.Count(errOut, "diag.db") != 1 || !strings.Contains(errOut, "skipping the diagnostic log") {
			t.Errorf("%v: code %d: %s %s", args, code, out, errOut)
		}
	}
}

// TestDiagSQLValueCap (T6777): one value prints at most sqlMaxValue
// bytes and a marker, in the table and in JSON, and a value past
// store.QueryMaxValue (randomblob(5e8)), a row wider than
// store.QueryMaxColumns, and the json_group aggregates are refused before
// they use the memory.
func TestDiagSQLValueCap(t *testing.T) {
	newBorrowFixture(t)
	big := "SELECT hex(zeroblob(50000)) AS v" // 100000 bytes
	marker := "…[truncated: 100000 bytes]"
	for _, args := range [][]string{{"sql", big}, {"sql", big, "--json"}} {
		code, out, errOut := runDiagCmd(args...)
		if code != 0 || !strings.Contains(out, marker) || len(out) > sqlMaxValue+200 {
			t.Errorf("%v: code %d, %d bytes out, marker %v: %q", args[2:], code, len(out), strings.Contains(out, marker), errOut)
		}
		if len(args) == 3 {
			var rows []map[string]string
			if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) != 1 || len(rows[0]["v"]) != sqlMaxValue+len(marker) {
				t.Errorf("--json: %v", err)
			}
		}
	}
	// D64-1: memory stays bounded. Each of these fails at once, with an
	// error, instead of building a value or row far past the limits.
	twenty := "SELECT " + strings.TrimSuffix(strings.Repeat("randomblob(16e6), ", 20), ", ")
	wide := "SELECT " + strings.TrimSuffix(strings.Repeat("1, ", store.QueryMaxColumns+1), ", ")
	cte := "WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM n WHERE x < 1000000) "
	for _, tc := range []struct{ name, q, want string }{
		{"randomblob(5e8)", "SELECT length(randomblob(5e8)) AS n, randomblob(5e8) AS b", "too big"},
		{"20 x randomblob(16e6)", twenty, "too big"},
		{"json_group_array", cte + "SELECT length(json_group_array(hex(zeroblob(1000)))) FROM n", "not authorized"},
		{"json_group_object", cte + "SELECT length(json_group_object(x, x)) FROM n", "not authorized"},
		{"too many columns", wide, "too many columns"},
	} {
		start := time.Now()
		code, out, errOut := runDiagCmd("sql", tc.q)
		if code == 0 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: code %d: %.200q %q, want %q", tc.name, code, out, errOut, tc.want)
		}
		if d := time.Since(start); d > 20*time.Second {
			t.Errorf("%s took %s", tc.name, d)
		}
	}
}

// TestDiagSQLCaps (P58-6): --limit stops at that many rows and says so;
// --timeout stops a query that runs on.
func TestDiagSQLCaps(t *testing.T) {
	newBorrowFixture(t)
	q := "WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM n WHERE x < 5) SELECT x FROM n"
	code, out, errOut := runDiagCmd("sql", q, "--limit", "2")
	if code != 0 || strings.Count(out, "\n") != 3 || !strings.Contains(errOut, "stopped at 2 rows") {
		t.Errorf("--limit 2: code %d: %q %q", code, out, errOut)
	}
	code, out, errOut = runDiagCmd("sql", q, "--limit", "2", "--json")
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); code != 0 || err != nil || len(rows) != 2 {
		t.Errorf("--json --limit 2: code %d: %q %q %v", code, out, errOut, err)
	}
	if code, out, errOut = runDiagCmd("sql", q, "--limit", "0"); code != 0 || strings.Count(out, "\n") != 6 || errOut != "" {
		t.Errorf("--limit 0: code %d: %q %q", code, out, errOut)
	}
	forever := "WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM n) SELECT count(*) FROM n"
	start := time.Now()
	if code, _, errOut = runDiagCmd("sql", forever, "--timeout", "200ms"); code == 0 || !strings.Contains(errOut, "--timeout") {
		t.Errorf("--timeout: code %d: %q", code, errOut)
	}
	if d := time.Since(start); d > 20*time.Second {
		t.Errorf("--timeout took %s", d)
	}
}
