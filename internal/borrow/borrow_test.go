package borrow

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

func TestPromptText(t *testing.T) {
	today := time.Date(2026, 10, 28, 23, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		p       Prompt
		want    []string
		notWant []string
	}{
		{"task and date", Prompt{Kind: adapter.News, Task: "what shipped in go 1.27?", Today: today},
			[]string{"Kind: news", "Today's date: 2026-10-28", "## Task\n\nwhat shipped in go 1.27?\n", "publish date"},
			[]string{"## Paths", "## Constraints"}},
		{"paths and constraints", Prompt{Kind: adapter.Review, Task: "review", Paths: []string{"/r/a.go", "/r/b"},
			Constraints: []string{"focus on races"}, Today: today},
			[]string{"## Paths\n\n- /r/a.go\n- /r/b\n", "## Constraints\n\n- focus on races\n", "Do not change any file"}, []string{"looked like secrets"}},
		// F3: every part is redacted before the prompt file is written.
		{"secrets", Prompt{Kind: adapter.Review, Task: "review\nexport OPENAI_API_KEY=sk-abcdefghijklmnop1234", Paths: []string{"/r/a.go", "/r/ghp_abcdefghijklmnopqrstuvwxyz0123"},
			Constraints: []string{"password: hunter2hunter2"}, Today: today,
			Material: &Material{Diff: "+db_password: correcthorse\n+ok line\n", Files: []MaterialFile{{Path: "/r/.env", Data: "AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY\n"}}}},
			[]string{"## Task\n\nreview\n", "- /r/a.go\n", "+ok line", "5 lines that looked like secrets", "removed from this prompt"},
			[]string{"sk-abcdef", "ghp_", "hunter2", "correcthorse", "wJalrXUtnFEMI", "## Constraints"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.p.Text()
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("prompt lacks %q:\n%s", w, got)
				}
			}
			for _, w := range tt.notWant {
				if strings.Contains(got, w) {
					t.Errorf("prompt has %q:\n%s", w, got)
				}
			}
		})
	}
}

func TestParseKind(t *testing.T) {
	tests := []struct {
		in      string
		want    adapter.Kind
		wantErr string
	}{
		{"image", adapter.Image, ""}, {"video", adapter.Video, ""}, {"research", adapter.Research, ""},
		{"news", adapter.News, ""}, {"review", adapter.Review, ""},
		{"edit", adapter.Edit, ""}, {"chat", "", "unknown kind"},
	}
	for _, tt := range tests {
		k, err := ParseKind(tt.in)
		if k != tt.want || (tt.wantErr == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
			t.Errorf("ParseKind(%q) = %q, %v", tt.in, k, err)
		}
	}
}

func TestRequestRefused(t *testing.T) {
	w := newWorld(t)
	b := w.borrower()
	tests := []struct {
		name string
		req  Request
	}{
		{"empty task", w.req(adapter.News, "  ")},
		{"edit outside a git work tree", Request{Kind: adapter.Edit, Task: "x", Repo: "/"}},
		{"relative path", Request{Kind: adapter.News, Task: "x", Repo: w.repo, Paths: []string{"a.go"}}},
		{"multi-line constraint", Request{Kind: adapter.News, Task: "x", Repo: w.repo, Constraints: []string{"a\nb"}}},
		{"huge task", w.req(adapter.News, strings.Repeat("x", MaxTask+1))},
		// #10-13: local paths never go to a web-facing child.
		{"research path", Request{Kind: adapter.Research, Task: "x", Repo: w.repo, Paths: []string{"/r/go.mod"}}},
		{"news path", Request{Kind: adapter.News, Task: "x", Repo: w.repo, Paths: []string{"/r/go.mod"}}},
		// T6709: only an edit child gets tools.
		{"news tools", Request{Kind: adapter.News, Task: "x", Repo: w.repo, Tools: []string{"context7"}}},
		{"review tools", func() Request { r := w.req(adapter.Review, "x"); r.Tools = []string{"context7"}; return r }()},
	}
	for _, tt := range tests {
		if _, err := b.Run(context.Background(), tt.req); err == nil {
			t.Errorf("%s: accepted", tt.name)
		}
	}
	if len(w.records()) != 0 {
		t.Error("a refused borrow ran a CLI")
	}
}

// TestPromptOnStdin (T6620): the prompt reaches the CLI on stdin, never in
// argv, and the borrow and its child are recorded.
func TestPromptOnStdin(t *testing.T) {
	w := newWorld(t)
	w.scenario[adapter.Grok] = `{"json":{"result":"Go 1.27 shipped."}}`
	task := "what shipped in go 1.27? SECRET-TASK-MARKER"
	req := w.req(adapter.News, task)
	req.Constraints = []string{"cite primary sources"}
	res, err := w.borrower().Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Status != "done" || res[0].Vendor != adapter.Grok || res[0].Text != "Go 1.27 shipped." {
		t.Fatalf("results = %+v", res)
	}
	recs := w.records()
	if len(recs) != 1 {
		t.Fatalf("runs = %d", len(recs))
	}
	r := recs[0]
	for _, a := range r.Args {
		if strings.Contains(a, "SECRET-TASK-MARKER") {
			t.Errorf("task in argv: %q", r.Args)
		}
	}
	for _, s := range []string{task, "cite primary sources", "2026-10-28"} {
		if !strings.Contains(r.Stdin+r.Prompt, s) {
			t.Errorf("prompt lacks %q: %q", s, r.Stdin+r.Prompt)
		}
	}
	got, err := w.store.GetBorrow(context.Background(), res[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" || got.Kind != "news" || got.Vendor != "grok" || got.ChildID == "" || got.Title == "" {
		t.Errorf("row = %+v", got)
	}
}

// TestRouterSkipsParent: the stand-in order skips the vendor on screen.
func TestRouterSkipsParent(t *testing.T) {
	w := newWorld(t)
	vs, _, err := w.router().Choose(context.Background(), RouteRequest{Kind: adapter.Image, Parent: adapter.Grok})
	if err != nil || !slices.Equal(vs, []adapter.Vendor{adapter.Codex}) {
		t.Errorf("image from grok = %v, %v", vs, err)
	}
	w.connected[adapter.Codex] = false
	if _, err := w.borrower().Run(context.Background(), Request{Kind: adapter.Image, Task: "x", Repo: w.repo, Parent: adapter.Grok}); err == nil || !strings.Contains(err.Error(), "no vendor available") {
		t.Errorf("no vendor: %v", err)
	}
}

// TestDelegated (T6623): every child runs with AGGY_DELEGATED=1, and a
// borrow under AGGY_DELEGATED is refused before anything runs.
func TestDelegated(t *testing.T) {
	w := newWorld(t)
	w.scenario[adapter.Grok] = `{"json":{"result":"ok"}}`
	if _, err := w.borrower().Run(context.Background(), w.req(adapter.News, "x")); err != nil {
		t.Fatal(err)
	}
	if got := w.records()[0].Env[Delegated]; got != "1" {
		t.Errorf("child %s = %q, want 1", Delegated, got)
	}
	// T6692: the child knows the vendor it runs as, for the commit trailer.
	if got := w.records()[0].Env["AGGY_VENDOR"]; got != "grok" {
		t.Errorf("child AGGY_VENDOR = %q, want grok", got)
	}
	b := w.borrower()
	b.Getenv = func(k string) string {
		if k == Delegated {
			return "1"
		}
		return ""
	}
	if _, err := b.Run(context.Background(), w.req(adapter.News, "x")); !errors.Is(err, ErrDelegated) {
		t.Errorf("err = %v, want ErrDelegated", err)
	}
	if len(w.records()) != 1 {
		t.Error("a delegated borrow ran a child")
	}
}

// TestReviewSkipsTrailerAuthors (T6693): a review's Authors reach the
// router, which passes over a vendor that wrote part of the change.
func TestReviewSkipsTrailerAuthors(t *testing.T) {
	w := newWorld(t)
	for _, v := range adapter.Vendors() {
		w.connected[v] = v == adapter.Grok || v == adapter.Claude
	}
	w.scenario[adapter.Grok] = `{"json":{"result":"grok reviewed"}}`
	w.scenario[adapter.Claude] = `{"json":{"type":"result","result":"claude reviewed"}}`
	req := w.req(adapter.Review, "review it")
	req.Authors = []adapter.Vendor{adapter.Grok}
	res, err := w.borrower().Run(context.Background(), req)
	if err != nil || len(res) != 1 || res[0].Vendor != adapter.Claude {
		t.Fatalf("results %+v, %v; want claude", res, err)
	}
}
