package borrow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/store"
)

// world is a borrow test: a temp HOME and state dir, a store, and the
// fake CLIs, every one connected unless a test says otherwise.
type world struct {
	t         *testing.T
	fakes     fakecli.Fakes
	home      string
	state     string
	repo      string
	record    string
	scenario  map[adapter.Vendor]string
	timeout   time.Duration // overrides the budget when set
	store     *store.Store
	cfg       config.Config
	connected map[adapter.Vendor]bool
	caps      map[adapter.Vendor][]string
	version   map[adapter.Vendor]string // "" means the tested version
	now       time.Time
}

func newWorld(t *testing.T) *world {
	t.Helper()
	fakes := fakecli.Build(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME", "AGGY_DELEGATED", "AGGY_VENDOR"} {
		t.Setenv(v, "")
	}
	state := filepath.Join(t.TempDir(), "aggy")
	t.Setenv("AGGY_HOME", state)
	s, err := store.Open(context.Background(), filepath.Join(state, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// The repo is a git work tree, as a review needs.
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &world{
		t: t, fakes: fakes, home: home, state: state, repo: repo,
		record:    filepath.Join(t.TempDir(), "record.jsonl"),
		scenario:  map[adapter.Vendor]string{},
		store:     s,
		cfg:       config.Default(),
		connected: map[adapter.Vendor]bool{adapter.Claude: true, adapter.Codex: true, adapter.Grok: true},
		caps:      map[adapter.Vendor][]string{},
		version:   map[adapter.Vendor]string{},
		now:       time.Date(2026, 10, 28, 9, 30, 0, 0, time.UTC),
	}
}

// fakeRouter is the stand-in router of internal/cli, over the world.
func (w *world) router() Router {
	return RouterFunc(func(_ context.Context, req RouteRequest) ([]adapter.Vendor, error) {
		var out []adapter.Vendor
		for _, v := range []adapter.Vendor{adapter.Grok, adapter.Codex, adapter.Claude} {
			if v != req.Parent && w.connected[v] && Supports(testAdapter(v, CLI{}), req.Kind) == nil {
				out = append(out, v)
			}
		}
		return out, nil
	})
}

func testAdapter(v adapter.Vendor, cli CLI) adapter.Adapter {
	switch v {
	case adapter.Claude:
		return claude.Adapter{Caps: cli.Caps}
	case adapter.Codex:
		return codex.New()
	}
	return grok.New()
}

func (w *world) borrower() *Borrower {
	return New(Deps{
		Store:   w.store,
		Router:  w.router(),
		Adapter: testAdapter,
		Locate: func(_ context.Context, v adapter.Vendor) (CLI, error) {
			version := w.version[v]
			if version == "" {
				version = testAdapter(v, CLI{}).TestedVersions().Max
			}
			return CLI{Path: w.fakes.For(string(v)), Version: version, Caps: w.caps[v]}, nil
		},
		Connected: func(_ context.Context, v adapter.Vendor) bool { return w.connected[v] },
		Runner:    w.runner,
		Config:    w.cfg,
		StateDir:  w.state,
		Now:       func() time.Time { return w.now },
		Getenv:    func(string) string { return "" },
	})
}

// runner is execrun.HeadlessRunner with the fakes' variables added and
// its timeout mapped onto ErrTimeout, as internal/cli does.
func (w *world) runner(timeout time.Duration) adapter.Runner {
	if w.timeout > 0 {
		timeout = w.timeout
	}
	run := execrun.HeadlessRunner(timeout)
	return func(ctx context.Context, c adapter.Command) (adapter.Output, error) {
		c.Env = append(append([]string{}, c.Env...), fakecli.EnvRecord+"="+w.record)
		vendor := adapter.Vendor(filepath.Base(c.Path)[len("fake-"):])
		if sc := w.scenario[vendor]; sc != "" {
			c.Env = append(c.Env, fakecli.EnvScenario+"="+fakecli.WriteScenario(w.t, sc))
		}
		out, err := run(ctx, c)
		if errors.Is(err, execrun.ErrTimeout) {
			err = fmt.Errorf("%w: %v", ErrTimeout, err)
		}
		return out, err
	}
}

// records returns the fakes' headless runs.
func (w *world) records() []fakecli.Record {
	var out []fakecli.Record
	for _, r := range fakecli.Records(w.t, w.record) {
		if r.Vendor != "hook" {
			out = append(out, r)
		}
	}
	return out
}

func (w *world) req(kind adapter.Kind, task string) Request {
	r := Request{Kind: kind, Task: task, Repo: w.repo}
	if kind == adapter.Review {
		r.Material = &Material{Base: "abc123", Diff: "--- a/x.go\n+++ b/x.go\n@@ -1 +1 @@\n-old\n+new\n"}
	}
	return r
}
