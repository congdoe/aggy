package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

// TestScreenEvent (T6925): a switch's launch writes a switch row, any other
// launch a screen row, and a restart of the same child for a tool change
// neither.
func TestScreenEvent(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	child := store.Child{ID: "c1", Vendor: "grok", Model: "grok-x"}
	tests := []struct {
		name   string
		t      target
		ok     bool
		typ    string
		detail map[string]any
	}{
		{"new session", target{New: true, Vendor: adapter.Grok}, true, store.EventScreen,
			map[string]any{"vendor": "grok", "model": "grok-x"}},
		{"switch", target{ParentID: "p1", Vendor: adapter.Grok, SwitchFrom: adapter.Claude, SwitchReason: "user"}, true, store.EventSwitch,
			map[string]any{"from": "claude", "to": "grok", "model": "grok-x", "reason": "user"}},
		{"back", target{ParentID: "p1", Vendor: adapter.Grok, SameChild: true, SwitchFrom: adapter.Codex, SwitchReason: "back: it never started"}, true, store.EventSwitch,
			map[string]any{"from": "codex", "to": "grok", "model": "grok-x", "reason": "back: it never started"}},
		{"tool restart", target{ParentID: "p1", Vendor: adapter.Grok, SameChild: true}, false, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, ok := screenEvent(tt.t, "p1", child, at)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if e.Type != tt.typ || e.ParentID != "p1" || e.ChildID != "c1" || !e.At.Equal(at) {
				t.Errorf("event = %+v", e)
			}
			for k, v := range tt.detail {
				if e.Detail[k] != v {
					t.Errorf("detail[%s] = %v, want %v", k, e.Detail[k], v)
				}
			}
		})
	}
}

// TestAcceptLogsFiles (T6925): an accepted edit borrow writes a files row
// under its parent, which aggy log prints; a declined accept writes none.
func TestAcceptLogsFiles(t *testing.T) {
	f, markers := editFixture(t)
	id, _ := editBorrow(t, f, map[string]string{"a.go": "package a\n\nfunc Edited() {}\n", "b.go": "package a\n"})
	gitIn(t, f.repo, "checkout", "--", "a.go")
	filesRows := func() []store.Event {
		t.Helper()
		ctx := context.Background()
		s, err := store.OpenDefault(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = s.Close() }()
		evs, err := s.Events(ctx, store.EventFilter{})
		if err != nil {
			t.Fatal(err)
		}
		var out []store.Event
		for _, e := range evs {
			if e.Type == store.EventFiles {
				out = append(out, e)
			}
		}
		return out
	}
	if code, out, _ := accept(f, "n\n", true, id); code != 1 || !strings.Contains(out, "Not applied.") {
		t.Fatalf("answered n: code %d: %s", code, out)
	}
	if rows := filesRows(); len(rows) != 0 {
		t.Fatalf("declined accept wrote %+v", rows)
	}
	if code, out, errOut := accept(f, "", false, id, "--yes"); code != 0 {
		t.Fatalf("accept: code %d: %s %s", code, out, errOut)
	}
	rows := filesRows()
	if len(rows) != 1 {
		t.Fatalf("files rows = %+v, want one", rows)
	}
	r := rows[0]
	var files []string
	for _, x := range r.Detail["files"].([]any) {
		files = append(files, x.(string))
	}
	if !slices.Equal(files, []string{"a.go", "b.go"}) || r.Detail["borrow"] != id || r.ChildID == "" || r.ParentID == "" {
		t.Errorf("files row = %+v", r)
	}
	var out, errb strings.Builder
	if code := runLog(nil, &out, &errb); code != 0 || !strings.Contains(out.String(), "a.go, b.go (accepted from borrow "+id+")") {
		t.Errorf("aggy log: code %d:\n%s%s", code, out.String(), errb.String())
	}
	noMarkers(t, markers)
}

// TestFilesRowCapped (T6925): a files row keeps at most maxLoggedFiles
// names and counts the rest, and aggy log quotes a name that a comma,
// quote, or edge space would make ambiguous.
func TestFilesRowCapped(t *testing.T) {
	s := sessionStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.InsertParent(ctx, store.Parent{ID: "aa11", Dir: t.TempDir(), CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.InsertChild(ctx, store.Child{ID: "c1", ParentID: "aa11", Vendor: "codex", Kind: "edit", Cwd: t.TempDir(), Status: "open", CreatedAt: now, LastActive: now}); err != nil {
		t.Fatal(err)
	}
	paths := []string{"a, b.go", `q"x.go`, " lead.go", "plain.go"}
	for i := len(paths); i < 120; i++ {
		paths = append(paths, fmt.Sprintf("f%03d.go", i))
	}
	if err := recordFiles(ctx, s, store.Borrow{ID: "b1", ChildID: "c1"}, paths, now); err != nil {
		t.Fatal(err)
	}
	evs, err := s.Events(ctx, store.EventFilter{})
	if err != nil || len(evs) != 1 {
		t.Fatalf("events %+v, %v", evs, err)
	}
	e := evs[0]
	if fs, _ := e.Detail["files"].([]any); len(fs) != maxLoggedFiles || e.Detail["more"] != float64(70) || e.ParentID != "aa11" {
		t.Fatalf("row %+v", e)
	}
	got := describe(e)
	for _, want := range []string{`"a, b.go", "q\"x.go", " lead.go", plain.go, f004.go`, "f049.go and 70 more (accepted from borrow b1)"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe = %q, want it to hold %q", got, want)
		}
	}
	if strings.Contains(got, "f050.go") {
		t.Errorf("describe shows past the cap: %q", got)
	}
	// An older or edited row past the cap shows the cap too.
	many := make([]any, 60)
	for i := range many {
		many[i] = fmt.Sprintf("g%02d", i)
	}
	if got := describe(store.Event{Type: store.EventFiles, Detail: map[string]any{"files": many}}); !strings.HasSuffix(got, "g49 and 10 more") {
		t.Errorf("long row = %q", got)
	}
}
