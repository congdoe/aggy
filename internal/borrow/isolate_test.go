package borrow

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestIsolation (T6621): every kind but review runs in a fresh empty
// temp dir outside the repo and the state dir, with the lockdown flags;
// review runs read-only in the repo. The temp dir is gone afterwards.
func TestIsolation(t *testing.T) {
	tests := []struct {
		name    string
		vendor  adapter.Vendor
		kind    adapter.Kind
		gitRepo bool
		inRepo  bool
		want    []string // args that must be present
		notWant []string
	}{
		{"grok news", adapter.Grok, adapter.News, false, false,
			[]string{"--tools", "web_search", "--no-subagents"}, []string{"--permission-mode"}},
		{"grok research", adapter.Grok, adapter.Research, false, false, []string{"web_search"}, nil},
		{"grok image", adapter.Grok, adapter.Image, false, false, []string{"image_gen", "--no-subagents"}, nil},
		{"grok video", adapter.Grok, adapter.Video, false, false, []string{"image_gen,image_to_video"}, nil},
		{"grok review", adapter.Grok, adapter.Review, true, true, []string{"plan", "read_file,grep,list_dir"}, nil},
		{"codex image", adapter.Codex, adapter.Image, false, false,
			[]string{"--ignore-user-config", "--skip-git-repo-check", "apps", "plugins"}, []string{"image_generation"}},
		{"codex review in a repo", adapter.Codex, adapter.Review, true, true,
			[]string{"--ignore-user-config", "image_generation", "shell_tool"}, []string{"--skip-git-repo-check"}},
		{"codex review outside a repo", adapter.Codex, adapter.Review, false, true, []string{"--skip-git-repo-check"}, nil},
		{"claude review", adapter.Claude, adapter.Review, true, true,
			[]string{"--strict-mcp-config", "--mcp-config", "plan", "Read,Grep,Glob"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			if tt.gitRepo {
				if err := os.Mkdir(filepath.Join(w.repo, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for _, v := range adapter.Vendors() {
				w.connected[v] = v == tt.vendor
			}
			w.scenario[adapter.Grok] = `{"json":{"result":"ok"}}`
			w.scenario[adapter.Claude] = `{"json":{"type":"result","result":"ok"}}`
			res, err := w.borrower().Run(context.Background(), w.req(tt.kind, "do it"))
			if err != nil {
				t.Fatal(err)
			}
			if res[0].Vendor != tt.vendor || res[0].Status != "done" {
				t.Fatalf("result %+v", res[0])
			}
			rec := w.records()[0]
			args := strings.Join(rec.Args, " ")
			for _, s := range tt.want {
				if !strings.Contains(args, s) {
					t.Errorf("args lack %q: %s", s, args)
				}
			}
			for _, s := range tt.notWant {
				if strings.Contains(args, s) {
					t.Errorf("args have %q: %s", s, args)
				}
			}
			if tt.inRepo {
				if rec.Cwd != w.repo {
					t.Errorf("review cwd = %s, want the repo %s", rec.Cwd, w.repo)
				}
			} else {
				if within(rec.Cwd, w.repo) || within(rec.Cwd, w.state) || within(rec.Cwd, w.home) || filepath.Base(rec.Cwd) != "work" {
					t.Errorf("cwd %s is not a fresh temp dir", rec.Cwd)
				}
				if _, err := os.Stat(rec.Cwd); !os.IsNotExist(err) {
					t.Errorf("temp dir left behind: %v", err)
				}
			}
			if tt.vendor == adapter.Claude {
				mcp := rec.Flags["--mcp-config"]
				if len(mcp) != 1 || filepath.Base(mcp[0]) != "mcp.json" || within(mcp[0], w.repo) {
					t.Errorf("--mcp-config %q", mcp)
				}
			}
		})
	}
}

// TestLockdownFailsBorrow (T6621): Grok's "name no tool" warning, printed
// on a successful exit, fails the borrow; nothing is retried.
func TestLockdownFailsBorrow(t *testing.T) {
	w := newWorld(t)
	w.scenario[adapter.Grok] = `{"json":{"result":"all tools were on"},"stderr":"warning: name no tool \"x\"\n"}`
	res, err := w.borrower().Run(context.Background(), w.req(adapter.News, "news"))
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Status != "failed" || res[0].Outcome != string(adapter.ErrLockdown) || !strings.Contains(res[0].Detail, "allowlist") {
		t.Errorf("result %+v", res[0])
	}
	if n := len(w.records()); n != 1 {
		t.Errorf("runs = %d, want 1 (no retry)", n)
	}
}

// TestTempDirInsideRepoRefused: a TMPDIR inside the repo is refused.
func TestTempDirInsideRepoRefused(t *testing.T) {
	w := newWorld(t)
	b := w.borrower()
	b.TempDir = filepath.Join(w.repo, "tmp")
	if err := os.Mkdir(b.TempDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Run(context.Background(), w.req(adapter.News, "x")); err == nil || !strings.Contains(err.Error(), "TMPDIR") {
		t.Errorf("err = %v", err)
	}
	if len(w.records()) != 0 {
		t.Error("ran a child")
	}
}

func TestWriteChildFilesRefusesPaths(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"../x", "a/b", "", ".hidden"} {
		err := writeChildFiles(fileAdapter{name: name}, adapter.HeadlessRequest{}, dir)
		if err == nil {
			t.Errorf("%q written", name)
		}
	}
	if err := writeChildFiles(fileAdapter{name: "ok.json"}, adapter.HeadlessRequest{}, dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || !slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.Name() == "ok.json" }) {
		t.Errorf("dir holds %v", entries)
	}
}

type fileAdapter struct {
	adapter.Adapter
	name string
}

func (f fileAdapter) ChildFiles(adapter.HeadlessRequest) ([]adapter.ChildFile, error) {
	return []adapter.ChildFile{{Name: f.name, Data: []byte("{}")}}, nil
}

// TestGuardReachesClaude (T6624): with a guard and the --settings
// capability, a Claude child gets the guard's settings layer.
func TestGuardReachesClaude(t *testing.T) {
	w := newWorld(t)
	w.connected[adapter.Grok], w.connected[adapter.Codex] = false, false
	w.caps[adapter.Claude] = []string{"settings"}
	w.scenario[adapter.Claude] = `{"extra_flags":["--settings"],"json":{"type":"result","result":"ok"}}`
	b := w.borrower()
	b.Guard = []string{"/usr/local/bin/aggy", "hook", "pretooluse"}
	res, err := b.Run(context.Background(), w.req(adapter.Review, "review it"))
	if err != nil || res[0].Status != "done" {
		t.Fatalf("%+v %v", res, err)
	}
	s := w.records()[0].Flags["--settings"]
	if len(s) != 1 || filepath.Base(s[0]) != "settings.json" || within(s[0], w.repo) {
		t.Errorf("--settings %q", s)
	}
}
