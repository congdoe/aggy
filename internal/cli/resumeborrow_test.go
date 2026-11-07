package cli

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/store"
)

// TestPlanBorrowChild (F13): aggy resume takes a borrow child's id, which
// aggy session lists; what cannot come back says why, and is never replaced
// by a new child.
func TestPlanBorrowChild(t *testing.T) {
	const (
		parent = "c0000000-0000-4000-8000-000000000000"
		id     = "abc1234500000000"
	)
	gone := "/nonexistent/aggy-borrow-1/work"
	tests := []struct {
		name   string
		child  func(dir string) store.Child
		tgt    target
		saved  bool // the vendor has the session's file
		want   string
		remake bool
		noFile bool // Grok's sessions folder is there, without this session
	}{
		{
			name: "a review resumes in its repo",
			child: func(dir string) store.Child {
				return store.Child{Kind: "review", Vendor: "claude", Status: "closed", Cwd: dir}
			},
			saved: true,
		},
		{
			name: "an edit resumes in its worktree",
			child: func(dir string) store.Child {
				return store.Child{Kind: "edit", Vendor: "claude", Status: "closed", Cwd: dir}
			},
			saved: true,
		},
		{
			name: "a research borrow's temp dir is made again",
			child: func(string) store.Child {
				return store.Child{Kind: "research", Vendor: "claude", Status: "closed", Cwd: gone}
			},
			saved: true, remake: true,
		},
		{
			name: "a removed worktree is refused",
			child: func(string) store.Child {
				return store.Child{Kind: "edit", Vendor: "claude", Status: "closed", Cwd: "/nonexistent/worktrees/abc"}
			},
			saved: true, want: "was removed",
		},
		{
			name: "a research dir that is not aggy's temp dir is refused",
			child: func(string) store.Child {
				return store.Child{Kind: "research", Vendor: "claude", Status: "closed", Cwd: "/nonexistent/home/work"}
			},
			saved: true, want: "was removed",
		},
		{
			name: "a running borrow is refused",
			child: func(dir string) store.Child {
				return store.Child{Kind: "research", Vendor: "claude", Status: "open", Cwd: dir}
			},
			saved: true, want: "still running",
		},
		{
			name: "no session id is refused",
			child: func(dir string) store.Child {
				return store.Child{Kind: "review", Vendor: "claude", Status: "closed", Cwd: dir}
			},
			saved: true, want: "no recorded session id",
		},
		{
			name: "a session the vendor lost is refused",
			child: func(dir string) store.Child {
				return store.Child{Kind: "review", Vendor: "grok", Status: "closed", Cwd: dir}
			},
			noFile: true, want: "no saved session",
		},
		{
			name: "a brief is refused",
			child: func(dir string) store.Child {
				return store.Child{Kind: "review", Vendor: "claude", Status: "closed", Cwd: dir}
			},
			tgt: target{Brief: true}, saved: true, want: "takes no brief",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)
			s := sessionStore(t)
			dir := t.TempDir()
			c := tt.child(dir)
			c.ID = id
			if tt.want != "no recorded session id" && !strings.Contains(tt.name, "no session id") {
				c.VendorSessionID = sidA
			}
			seedParent(t, s, parent, dir, store.Child{ID: "chat", Vendor: "claude", VendorSessionID: sidB}, c)
			if tt.saved {
				claudeSession(t, sidA, time.Now())
			}
			claudeSession(t, sidB, time.Now())
			if tt.noFile {
				if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".grok", "sessions"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			tgt := tt.tgt
			tgt.ParentID = id[:6]
			p, err := plan(context.Background(), s, config.Default(), dir, tgt, time.Now(), &bytes.Buffer{})
			if tt.want != "" {
				if err == nil || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("plan = %+v, %v; want %q", p, err, tt.want)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.newParent || p.newChild || p.child.ID != id || p.resume != sidA || p.remakeCwd != tt.remake {
				t.Errorf("plan = %+v", p)
			}
		})
	}
}

// TestPlanSwitchNeverPicksBorrow: a switch's child id comes over the socket,
// and a switch picks among interactive children only.
func TestPlanSwitchNeverPicksBorrow(t *testing.T) {
	isolate(t)
	s := sessionStore(t)
	dir := t.TempDir()
	const parent = "c0000000-0000-4000-8000-000000000000"
	seedParent(t, s, parent, dir, store.Child{ID: "b1", Vendor: "claude", Kind: "review", Status: "closed", VendorSessionID: sidA, Cwd: dir})
	claudeSession(t, sidA, time.Now())
	_, err := plan(context.Background(), s, config.Default(), dir, target{ParentID: parent, Vendor: adapter.Claude, Child: "b1"}, time.Now(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "no longer recorded") {
		t.Errorf("err = %v, want the child refused", err)
	}
}

// TestRemakeBorrowDir: the dir is made new and private, removed after, and
// never used when any of it is already there: what is there may not be aggy's.
func TestRemakeBorrowDir(t *testing.T) {
	isolate(t)
	cwd := filepath.Join(t.TempDir(), "aggy-borrow-1", "work")
	remove, err := remakeBorrowDir(cwd)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(cwd)
	if err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("cwd = %v, %v", fi, err)
	}
	if _, err := remakeBorrowDir(cwd); err == nil || !strings.Contains(err.Error(), "already there") {
		t.Errorf("second err = %v, want already there", err)
	}
	remove()
	if _, err := os.Lstat(filepath.Dir(cwd)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("root after remove: %v", err)
	}
	// A planted dir, with a planted instructions file, is refused and kept.
	if err := os.MkdirAll(cwd, 0o777); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(cwd, "CLAUDE.md")
	if err := os.WriteFile(planted, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := remakeBorrowDir(cwd); err == nil {
		t.Error("a planted dir was accepted")
	}
	if _, err := os.Stat(planted); err != nil {
		t.Errorf("planted file: %v", err)
	}
}
