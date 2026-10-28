package borrow

import (
	"context"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestTaskKey (L48-1): the task hash covers what makes a task different,
// not just its text.
func TestTaskKey(t *testing.T) {
	base := func() Request {
		return Request{Kind: adapter.Review, Task: "Review the diff", Constraints: []string{"be brief", "cite lines"},
			Paths: []string{"/repo/a.go"}, Material: &Material{Base: "abc", Diff: "-old\n+new\n"}}
	}
	k0 := taskKey(base(), "")
	tests := []struct {
		name     string
		edit     func(*Request)
		editBase string
		same     bool
	}{
		{"case, spacing, and constraint order", func(r *Request) {
			r.Task = "  review THE   diff"
			r.Constraints = []string{"Cite  lines", "be brief"}
			r.Paths = []string{"/repo/./a.go"}
		}, "", true},
		{"another diff", func(r *Request) { r.Material.Diff = "-old\n+newer\n" }, "", false},
		{"another review base", func(r *Request) { r.Material.Base = "def" }, "", false},
		{"another file's content", func(r *Request) { r.Material.Files = []MaterialFile{{Path: "/repo/a.go", Data: "x"}} }, "", false},
		{"another constraint", func(r *Request) { r.Constraints = []string{"be brief", "cite files"} }, "", false},
		{"one constraint fewer", func(r *Request) { r.Constraints = r.Constraints[:1] }, "", false},
		{"another path", func(r *Request) { r.Paths = []string{"/repo/b.go"} }, "", false},
		{"another kind", func(r *Request) { r.Kind = adapter.Research }, "", false},
		{"another edit base", func(*Request) {}, "c0ffee", false},
		{"another --out", func(r *Request) { r.Media = &Media{Out: "/repo/b.png"} }, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := base()
			tt.edit(&r)
			if got := taskKey(r, tt.editBase) == k0; got != tt.same {
				t.Errorf("same key = %v, want %v", got, tt.same)
			}
		})
	}
	a, b := base(), base()
	a.Media, b.Media = &Media{Out: "/repo/a.png"}, &Media{Out: "/repo/b.png"}
	if taskKey(a, "") == taskKey(b, "") {
		t.Error("two --out paths share a key")
	}
}

// TestRedoNeedsTheSameTask (L48-1): the same review text on another diff,
// and the same question under other constraints, are not redos; the same
// review of the same diff is.
func TestRedoNeedsTheSameTask(t *testing.T) {
	w := newWorld(t)
	for _, v := range adapter.Vendors() {
		w.connected[v] = v == adapter.Grok
	}
	w.scenario[adapter.Grok] = `{"json":{"result":"ok"}}`
	b := w.borrower()
	run := func(r Request) {
		t.Helper()
		w.now = w.now.Add(time.Hour)
		if _, err := b.Run(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	redone := func() map[adapter.Kind]int {
		t.Helper()
		st, err := w.store.BorrowStats(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		out := map[adapter.Kind]int{}
		for _, s := range st {
			out[adapter.Kind(s.Kind)] += s.Redone
		}
		return out
	}
	reviewOn := func(diff string) Request {
		r := w.req(adapter.Review, "review the diff")
		r.Material.Diff = diff
		return r
	}
	research := func(c string) Request {
		r := w.req(adapter.Research, "what changed")
		r.Constraints, r.Fresh = []string{c}, true
		return r
	}
	run(reviewOn("-a\n+b\n"))
	run(reviewOn("-b\n+c\n"))
	run(research("as of 2026"))
	run(research("as of 2025"))
	if got := redone(); got[adapter.Review] != 0 || got[adapter.Research] != 0 {
		t.Fatalf("redone = %v, want none", got)
	}
	run(reviewOn("-b\n+c\n"))
	if got := redone(); got[adapter.Review] != 1 {
		t.Errorf("the same review again: redone = %v, want 1 review", got)
	}
}
