package borrow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestResearchCache (T6641): the same question reuses its report for a
// day (news) or a week (research) without running a child, unless
// --fresh; anything else that differs asks again.
func TestResearchCache(t *testing.T) {
	const task = "What shipped in Go 1.27?"
	tests := []struct {
		name   string
		kind   adapter.Kind
		later  time.Duration
		change func(w *world, r *Request)
		hit    bool
	}{
		{"same question", adapter.News, time.Hour, nil, true},
		{"case and spaces", adapter.News, time.Hour, func(_ *world, r *Request) { r.Task = "  what shipped in\tgo 1.27?" }, true},
		{"--fresh", adapter.News, time.Hour, func(_ *world, r *Request) { r.Fresh = true }, false},
		{"news after a day", adapter.News, 25 * time.Hour, nil, false},
		{"research within a week", adapter.Research, 6 * 24 * time.Hour, nil, true},
		{"research after a week", adapter.Research, 8 * 24 * time.Hour, nil, false},
		{"clock set back", adapter.News, -time.Hour, nil, false},
		{"other constraints", adapter.News, time.Hour, func(_ *world, r *Request) { r.Constraints = []string{"only blogs"} }, false},
		{"other kind", adapter.News, time.Hour, func(_ *world, r *Request) { r.Kind = adapter.Research }, false},
		{"the same vendor asked for", adapter.News, time.Hour, func(_ *world, r *Request) { r.Vendor = "grok" }, true},
		{"another repo", adapter.News, time.Hour, func(w *world, r *Request) {
			r.Repo = w.t.TempDir()
			if err := os.Mkdir(filepath.Join(r.Repo, ".git"), 0o700); err != nil {
				w.t.Fatal(err)
			}
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.scenario[adapter.Grok] = `{"json":{"result":"## Summary\nGo 1.27 shipped."}}`
			req := w.req(tt.kind, task)
			first, err := w.borrower().Run(context.Background(), req)
			if err != nil || len(first) != 1 || first[0].ResearchPath == "" {
				t.Fatalf("first run: %+v, %v", first, err)
			}
			w.now = w.now.Add(tt.later)
			if tt.change != nil {
				tt.change(w, &req)
			}
			res, err := w.borrower().Run(context.Background(), req)
			if err != nil || len(res) != 1 || res[0].Status != "done" {
				t.Fatalf("second run: %+v, %v", res, err)
			}
			runs := len(w.records())
			if tt.hit {
				if runs != 1 || res[0].CachedAt.IsZero() || res[0].ID != first[0].ID || res[0].Text != "## Summary\nGo 1.27 shipped." {
					t.Fatalf("want a cache hit: runs %d, result %+v", runs, res[0])
				}
				if !strings.HasPrefix(res[0].ResearchPath, filepath.Join(req.Repo, ".aggy", "research")) {
					t.Errorf("cached report at %s, not in %s", res[0].ResearchPath, req.Repo)
				}
				b, err := os.ReadFile(res[0].ResearchPath)
				if err != nil || !strings.Contains(string(b), "Go 1.27 shipped.") {
					t.Errorf("cached report copy: %q, %v", b, err)
				}
				return
			}
			if runs != 2 || !res[0].CachedAt.IsZero() {
				t.Errorf("want a new run: runs %d, result %+v", runs, res[0])
			}
		})
	}
}

// TestResearchCacheRouting: a cached report is not used where routing
// would not pick its vendor now, such as with the parent on that vendor;
// and an entry that does not match its question is ignored.
func TestResearchCacheRouting(t *testing.T) {
	w := newWorld(t)
	w.scenario[adapter.Grok] = `{"json":{"result":"news"}}`
	req := w.req(adapter.News, "x")
	first, err := w.borrower().Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	req.Parent = adapter.Grok
	if res, err := w.borrower().Run(context.Background(), req); err == nil || len(res) != 0 {
		t.Errorf("parent on grok got %+v, %v", res, err)
	}

	// An entry whose key is not its question's.
	req.Parent = ""
	p := w.borrower().cachePath(cacheKey(req))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), first[0].ID, "zz", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if res, err := w.borrower().Run(context.Background(), req); err != nil || !res[0].CachedAt.IsZero() {
		t.Errorf("a broken entry was used: %+v, %v", res, err)
	}
}
