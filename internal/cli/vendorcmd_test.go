package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/store"
)

// TestVendorCommandRefuses: a flag that clashes never reaches a launch: it
// exits 2 with its code, before any terminal or index is looked at.
func TestVendorCommandRefuses(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
		code string
	}{
		{"session id", []string{"claude", "--session-id", "x"}, "--session-id clashes", "cli.flag_clash"},
		{"settings", []string{"claude", "--settings", "{}"}, "--settings clashes", "cli.flag_clash"},
		{"codex memory", []string{"codex", "-c", "developer_instructions=x"}, "shared memory", "cli.flag_clash"},
		{"grok deny", []string{"grok", "--deny", "x"}, "--deny clashes", "cli.flag_clash"},
		{"a prompt", []string{"claude", "fix it"}, "not a flag", "cli.usage"},
		{"a prompt after a switch", []string{"claude", "--verbose", "fix it"}, "not a flag", "cli.usage"},
		{"a subcommand", []string{"codex", "exec"}, "not a flag", "cli.usage"},
		{"--", []string{"claude", "--", "fix"}, "starts a prompt", "cli.usage"},
		{"a loosening flag", []string{"claude", "--dangerously-skip-permissions"}, "--no-policy", "cli.flag_needs_no_policy"},
		{"a loosening -c", []string{"codex", "-c", `sandbox_mode="danger-full-access"`}, "--no-policy", "cli.flag_needs_no_policy"},
		{"both", []string{"claude", "-c", "-r"}, "not both", "cli.usage"},
		{"a resume id that could be a path", []string{"claude", "-r", "../x"}, "not a session id", "cli.usage"},
		{"a tool with no name", []string{"claude", "--tool"}, "needs a name", "cli.usage"},
		{"a bad tool name", []string{"grok", "--tool=A B"}, "not a tool name", "cli.usage"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			t.Setenv("PATH", t.TempDir())
			var out, errb bytes.Buffer
			if code := Run(tt.args, &out, &errb); code != 2 {
				t.Errorf("exit = %d, want 2", code)
			}
			if !strings.Contains(errb.String(), tt.want) || !strings.Contains(errb.String(), "["+tt.code+" · run ") {
				t.Errorf("stderr = %q, want %q and %s", errb.String(), tt.want, tt.code)
			}
		})
	}
}

// TestPlanContinue: bare aggy starts a parent of its own each time, so
// --continue looks through this directory's parents for the session it can
// reopen, and refuses when there is none, as claude --continue does.
func TestPlanContinue(t *testing.T) {
	const (
		older = "a0000000-0000-4000-8000-000000000000"
		newer = "b0000000-0000-4000-8000-000000000000"
	)
	two := func(t *testing.T, s *store.Store, dir string) {
		seedParent(t, s, older, dir, store.Child{ID: "old-claude", Vendor: "claude", VendorSessionID: sidA})
		claudeSession(t, sidA, time.Now().Add(-time.Hour))
		seedParent(t, s, newer, dir, store.Child{ID: "new-grok", Vendor: "grok", VendorSessionID: sidB})
		if err := s.SetChildActivity(context.Background(), "new-grok", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name   string
		seed   func(t *testing.T, s *store.Store, dir string)
		vendor adapter.Vendor
		want   string // the child; "" means an error
	}{
		{name: "nothing here", seed: func(*testing.T, *store.Store, string) {}},
		{
			name: "another directory's session is not continued",
			seed: func(t *testing.T, s *store.Store, _ string) {
				seedParent(t, s, older, t.TempDir(), store.Child{ID: "x", Vendor: "claude", VendorSessionID: sidA})
				claudeSession(t, sidA, time.Now())
			},
		},
		{
			name: "a vendor's latest is found in an older parent",
			seed: two, vendor: adapter.Claude, want: "old-claude",
		},
		{
			name: "a session never saved is skipped for the one before it",
			seed: func(t *testing.T, s *store.Store, dir string) {
				two(t, s, dir)
				// Grok's folder exists and holds nothing for sidB: gone.
				if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".grok", "sessions"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			want: "old-claude",
		},
		{
			name: "a borrow child is not continued",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, older, dir, store.Child{ID: "b", Vendor: "claude", Kind: "research", VendorSessionID: sidA})
				claudeSession(t, sidA, time.Now())
			},
		},
		{
			name: "no session of that vendor",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, older, dir, store.Child{ID: "c", Vendor: "claude", VendorSessionID: sidA})
				claudeSession(t, sidA, time.Now())
			},
			vendor: adapter.Grok,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			s := sessionStore(t)
			dir := t.TempDir()
			tt.seed(t, s, dir)
			p, err := plan(context.Background(), s, config.Default(), dir, target{Continue: true, Vendor: tt.vendor}, time.Now(), &bytes.Buffer{})
			if tt.want == "" {
				if !errors.Is(err, errNoSession) {
					t.Fatalf("plan = %+v, %v; want errNoSession", p, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.newParent || p.newChild || p.child.ID != tt.want || p.resume == "" {
				t.Errorf("plan = %+v, want to resume %s", p, tt.want)
			}
		})
	}
}

// TestSameChildAgain (T6903): a tool-change resume drops --no-policy (S-6), so
// the flags only it let through stay behind; the rest are the same session's.
func TestSameChildAgain(t *testing.T) {
	first := target{New: true, Vendor: adapter.Codex, NoPolicy: true, Model: "m", Tools: []string{"x"},
		Extra: []string{"--search", "--yolo"}, SafeExtra: []string{"--search"}, Loosens: []string{"--yolo"}}
	again := sameChildAgain(first, switchTo{vendor: adapter.Codex, parentID: "p", child: "c"})
	if again.NoPolicy || len(again.Loosens) != 0 || !slices.Equal(again.Extra, []string{"--search"}) {
		t.Errorf("target = %+v, want --yolo gone with --no-policy", again)
	}
	if again.Model != "m" || !slices.Equal(again.Tools, []string{"x"}) || again.Child != "c" || !again.SameChild {
		t.Errorf("target = %+v, want the same session", again)
	}
}

// TestLaunchCode: each kind of launch refusal has its own code.
func TestLaunchCode(t *testing.T) {
	tests := []struct {
		name string
		seed func(t *testing.T, s *store.Store, dir string)
		tgt  target
		want diag.Code
	}{
		{name: "no session", seed: func(*testing.T, *store.Store, string) {}, tgt: target{ParentID: "abcd"}, want: diag.CodeNotFound},
		{
			name: "ambiguous",
			seed: func(t *testing.T, s *store.Store, dir string) {
				seedParent(t, s, "abcd0000-0000-4000-8000-000000000000", dir)
				seedParent(t, s, "abcd1111-0000-4000-8000-000000000000", dir)
			},
			tgt: target{ParentID: "abcd"}, want: diag.CodeAmbiguousID,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := sessionStore(t)
			dir := t.TempDir()
			tt.seed(t, s, dir)
			_, err := plan(context.Background(), s, config.Default(), dir, tt.tgt, time.Now(), &bytes.Buffer{})
			if got := launchCode(err, diag.CodeLaunchFailed); got != tt.want {
				t.Errorf("code = %s, want %s (err %v)", got, tt.want, err)
			}
		})
	}
	t.Run("allowed_vendors", func(t *testing.T) {
		isolate(t)
		dir := t.TempDir()
		mustWrite(t, filepath.Join(dir, ".aggy", "config.toml"), "allowed_vendors = [\"grok\"]\n", 0o600)
		_, err := allowedIn(config.Default(), dir, adapter.Claude)
		if got := launchCode(err, diag.CodeConfig); got != diag.CodeNotAllowed {
			t.Errorf("code = %s (err %v)", got, err)
		}
	})
}

// TestPlanBareIsFresh: the target bare aggy asks for starts a new parent even with
// a session here to resume.
func TestPlanBareIsFresh(t *testing.T) {
	isolate(t)
	s := sessionStore(t)
	dir := t.TempDir()
	seedParent(t, s, "a0000000-0000-4000-8000-000000000000", dir, store.Child{ID: "c", Vendor: "grok", VendorSessionID: sidB})
	cfg := config.Default()
	cfg.DefaultVendor = adapter.Claude
	p, err := plan(context.Background(), s, cfg, dir, target{New: true}, time.Now(), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !p.newParent || !p.newChild || p.resume != "" || p.child.Vendor != "claude" {
		t.Errorf("plan = %+v, want a new parent on the default vendor, not the last vendor used", p)
	}
}

// TestGateDirFlags (T6952): a folder flag is judged by the folder it reaches
// from the directory the CLI starts in, not by how it is spelled.
func TestGateDirFlags(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	child := filepath.Join(home, "a", "b")
	other := filepath.Join(root, "srv", "x", "y")
	for _, d := range []string{child, other, filepath.Join(home, ".ssh"), filepath.Join(home, ".codex")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		t.Setenv(v, "")
	}
	toHome := filepath.Join(root, "to-home")
	if err := os.Symlink(home, toHome); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		dir   string
		cwd   string
		gated bool
	}{
		{"home", home, other, true},
		{"the root", "/", other, true},
		{"a link to home", toHome, other, true},
		{"the keys", filepath.Join(home, ".ssh"), other, true},
		{"a vendor's folder", filepath.Join(home, ".codex"), other, true},
		{"tilde", "~", other, true},
		{"tilde slash", "~/", other, true},
		{"tilde keys", "~/.ssh", other, true},
		{"another's tilde", "~root", other, true},
		{"relative, up to home from the child's cwd", "../..", child, true},
		{"the same word from elsewhere", "../..", other, false},
		{"a project", other, child, false},
		{"under home", "~/a", other, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := adapter.DirFlag{Name: "--add-dir", Value: tt.dir}
			safe := []string{"--search", "--add-dir", tt.dir}
			tg := target{Dirs: []adapter.DirFlag{d}, SafeExtra: safe}
			err := gateDirFlags(&tg, tt.cwd)
			if got := errors.Is(err, adapter.ErrLoosens); got != tt.gated {
				t.Fatalf("err = %v, want gated %v", err, tt.gated)
			}
			tg = target{NoPolicy: true, Dirs: []adapter.DirFlag{d}, SafeExtra: safe}
			if err := gateDirFlags(&tg, tt.cwd); err != nil {
				t.Fatal(err)
			}
			wantSafe := safe
			var wantLoosens []string
			if tt.gated {
				wantSafe, wantLoosens = []string{"--search"}, []string{"--add-dir"}
			}
			if !slices.Equal(tg.SafeExtra, wantSafe) || !slices.Equal(tg.Loosens, wantLoosens) {
				t.Errorf("with --no-policy: safe %q, loosens %q", tg.SafeExtra, tg.Loosens)
			}
		})
	}
}
