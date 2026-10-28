package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/store"
)

func TestMain(m *testing.M) { os.Exit(fakecli.IsolatedMain(m)) }

// testWriter is a writer whose diag.db and index are in a temp dir.
func testWriter(t *testing.T, level string) (*Writer, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	var warn bytes.Buffer
	w := New("v-test", &warn)
	w.diagPath = func() (string, error) { return filepath.Join(dir, "diag.db"), nil }
	w.indexPath = func() (string, error) { return filepath.Join(dir, "aggy.db"), nil }
	w.logConfig = func() (config.Log, error) { return config.Log{Level: level}, nil }
	w.getenv = func(string) string { return "" }
	return w, &warn, dir
}

func records(t *testing.T, dir string) []store.DiagRecord {
	t.Helper()
	v, err := store.OpenDiagView(context.Background(), filepath.Join(dir, "diag.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v.Close() }()
	recs, err := v.Records(context.Background(), store.DiagFilter{})
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestULID(t *testing.T) {
	var g ulids
	now := time.Now()
	var ids []string
	for i := 0; i < 1000; i++ {
		ids = append(ids, g.next(now)) // one millisecond: counts up
	}
	ids = append(ids, g.next(now.Add(time.Second)))
	for _, id := range ids {
		if !ValidID(id) {
			t.Fatalf("%q is not a valid id", id)
		}
	}
	if !sort.StringsAreSorted(ids) {
		t.Error("ids are not in order")
	}
	for _, bad := range []string{"", "01JBX4M6T0Q2W8Y5C3N7R9D1F", "81JBX4M6T0Q2W8Y5C3N7R9D1FA", "01JBX4M6T0Q2W8Y5C3N7R9D1FI", "01jbx4m6t0q2w8y5c3n7r9d1fa"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
}

func TestWriterRecord(t *testing.T) {
	w, warn, dir := testWriter(t, "info")
	task := "Summarize the canary-zebra-7731 report\nwith all its sections please"
	w.Forbid(task)
	id := w.Log(context.Background(), Rec{
		Event: EvBorrowRefused, Code: CodeBorrowStart,
		Msg:    "failed on canary-zebra-7731 report\x1b[31m\nAWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Attrs:  map[string]any{"why": "with all its sections please", "n": 3, "ok": true},
		Borrow: "b1", Child: "c1", Parent: "p1", Vendor: "grok", Exit: Int(2), Duration: 1500 * time.Millisecond,
	})
	if id == "" || !ValidID(id) {
		t.Fatalf("Log = %q, warn %q", id, warn)
	}
	recs := records(t, dir)
	if len(recs) != 1 {
		t.Fatalf("got %d records", len(recs))
	}
	r := recs[0]
	if r.ID != id || r.Level != "error" || r.Component != "borrow" || r.Code != string(CodeBorrowStart) ||
		r.RunID != w.RunID() || r.TraceID != w.Trace() || r.BorrowID != "b1" || r.Schema != Schema ||
		*r.ExitCode != 2 || *r.DurationMS != 1500 || r.AggyVersion != "v-test" {
		t.Errorf("record = %+v", r)
	}
	all := r.Msg + string(r.Attrs)
	for _, leak := range []string{"canary-zebra", "wJalrXUtnFEMI", "\x1b", "with all its sections"} {
		if strings.Contains(all, leak) {
			t.Errorf("%q stored: %s", leak, all)
		}
	}
	var attrs map[string]any
	if err := json.Unmarshal(r.Attrs, &attrs); err != nil || attrs["n"] != float64(3) || attrs["ok"] != true {
		t.Errorf("attrs = %s (%v)", r.Attrs, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "diag.db")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("diag.db mode: %v, %v", fi, err)
	}
}

func TestWriterTrace(t *testing.T) {
	valid := "01JBX4M6T0Q2W8Y5C3N7R9D1FA"
	for _, tt := range []struct {
		env  string
		want func(*Writer) string
	}{
		{"", func(w *Writer) string { return w.RunID() }},
		{valid, func(*Writer) string { return valid }},
		{"not-a-ulid; rm -rf", func(w *Writer) string { return w.RunID() }},
	} {
		t.Setenv(TraceEnv, tt.env)
		w := New("v", nil)
		if w.Trace() != tt.want(w) {
			t.Errorf("AGGY_TRACE=%q: trace %q", tt.env, w.Trace())
		}
	}
}

// TestWriterBorrowTrace: a later run's record about a borrow joins the
// borrow's trace.
func TestWriterBorrowTrace(t *testing.T) {
	a, _, dir := testWriter(t, "info")
	a.Log(context.Background(), Rec{Event: EvBorrowChildStarted, Borrow: "b9"})
	b := New("v", nil)
	b.diagPath, b.indexPath, b.logConfig, b.getenv = a.diagPath, a.indexPath, a.logConfig, a.getenv
	b.Log(context.Background(), Rec{Event: EvAcceptApplied, Borrow: "b9"})
	b.Log(context.Background(), Rec{Event: EvGCRemoved})
	recs := records(t, dir)
	if len(recs) != 3 || recs[1].TraceID != a.Trace() || recs[1].RunID != b.RunID() || recs[2].TraceID != b.Trace() {
		t.Errorf("records = %+v", recs)
	}
}

func TestWriterLevel(t *testing.T) {
	w, _, dir := testWriter(t, "warn")
	w.Log(context.Background(), Rec{Event: EvBorrowChildStarted}) // info
	w.Log(context.Background(), Rec{Event: EvBorrowFailed})       // warn
	if recs := records(t, dir); len(recs) != 1 || recs[0].Event != string(EvBorrowFailed) {
		t.Errorf("records = %+v", recs)
	}
	if w.stores(LevelDebug) {
		t.Error("debug stored without AGGY_DEBUG")
	}
	w.getenv = func(k string) string { return map[string]string{DebugEnv: "1"}[k] }
	if !w.stores(LevelDebug) {
		t.Error("debug not stored with AGGY_DEBUG=1")
	}
}

// TestWriterFailure: a write that fails returns "" and warns once, with
// the code and run id; the command goes on.
func TestWriterFailure(t *testing.T) {
	w, warn, dir := testWriter(t, "info")
	block := filepath.Join(dir, "file")
	if err := os.WriteFile(block, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w.diagPath = func() (string, error) { return filepath.Join(block, "diag.db"), nil }
	for i := 0; i < 3; i++ {
		if id := w.Log(context.Background(), Rec{Event: EvGCRemoved}); id != "" {
			t.Errorf("Log = %q", id)
		}
	}
	w.diagPath = func() (string, error) { return "", errors.New("no state dir") }
	w.Log(context.Background(), Rec{Event: EvGCRemoved})
	if n := strings.Count(warn.String(), "aggy: warning"); n != 1 || !strings.Contains(warn.String(), "[diag.write_failed · run "+w.RunID()+"]") {
		t.Errorf("warnings = %q", warn)
	}
	// A path that works again (the kept handle follows the path).
	for _, sub := range []string{"a", "b"} {
		p := filepath.Join(dir, sub, "diag.db")
		w.diagPath = func() (string, error) { return p, nil }
		if id := w.Log(context.Background(), Rec{Event: EvGCRemoved}); id == "" {
			t.Errorf("%s: not written", sub)
		}
		if recs := records(t, filepath.Dir(p)); len(recs) != 1 {
			t.Errorf("%s: %d records", sub, len(recs))
		}
	}
}

func TestWriterUnregistered(t *testing.T) {
	w, warn, dir := testWriter(t, "info")
	if id := w.Log(context.Background(), Rec{Event: "made.up.event"}); id != "" {
		t.Errorf("unregistered event stored: %q", id)
	}
	if id := w.Log(context.Background(), Rec{Event: EvGCRemoved, Code: "made.up"}); id != "" {
		t.Errorf("unregistered code stored: %q", id)
	}
	if !strings.Contains(warn.String(), "unregistered event") {
		t.Errorf("warn = %q", warn)
	}
	if _, err := os.Stat(filepath.Join(dir, "diag.db")); err == nil {
		t.Error("diag.db made for nothing")
	}
	var nilW *Writer
	if nilW.Log(context.Background(), Rec{Event: EvGCRemoved}) != "" || nilW.Tag(CodeUsage) != "" || nilW.Trace() != "" {
		t.Error("a nil writer wrote")
	}
}

// TestWriterSecurityRefusal: a security code also becomes a refusal row
// in the index's events; another code does not.
func TestWriterSecurityRefusal(t *testing.T) {
	w, warn, dir := testWriter(t, "info")
	rid := w.Log(context.Background(), Rec{Event: EvGrokLockdown, Code: CodeLockdown, Borrow: "b1", Child: "c1", Vendor: "grok"})
	w.Log(context.Background(), Rec{Event: EvBorrowFailed, Code: CodeRateLimit})
	s, err := store.OpenReadOnly(context.Background(), filepath.Join(dir, "aggy.db"))
	if err != nil {
		t.Fatalf("%v (warn %q)", err, warn)
	}
	defer func() { _ = s.Close() }()
	evs, err := s.Events(context.Background(), store.EventFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Type != store.EventRefusal || evs[0].ChildID != "c1" ||
		evs[0].Detail["code"] != string(CodeLockdown) || evs[0].Detail["record"] != rid || evs[0].Detail["run"] != w.RunID() {
		t.Errorf("events = %+v", evs)
	}
}

func TestMarkdownEscapes(t *testing.T) {
	md := Markdown()
	if strings.Contains(md, "<run id>") || !strings.Contains(md, "&lt;run id&gt;") {
		t.Error("Markdown does not escape <...>")
	}
	if got := strings.Count(md, "\n| `"); got != len(events)+len(codes) {
		t.Errorf("Markdown has %d rows, want %d", got, len(events)+len(codes))
	}
}
