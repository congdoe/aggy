//go:build unix

package borrow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

// linkedWorktrees hands out a worktree spelled through a symlink.
type linkedWorktrees struct{ link string }

func (l linkedWorktrees) Create(_ context.Context, repo, id string) (store.Worktree, error) {
	p := filepath.Join(l.link, id)
	return store.Worktree{Path: p, Branch: "aggy/edit-" + id, Base: strings.Repeat("a", 40), Repo: repo, State: store.WorktreePending}, os.Mkdir(p, 0o700)
}
func (linkedWorktrees) Remove(context.Context, store.Worktree) error { return nil }
func (linkedWorktrees) Tree(context.Context, store.Worktree) (string, error) {
	return strings.Repeat("b", 40), nil
}

// TestEditChildGetsThePhysicalPath (T6938): Codex's Seatbelt refuses a writable
// root with a symlink in it, and a refused child can still report success, so
// -C, the trust key, and the child's directory are all the resolved path. The
// borrow's own record keeps the worktree's path as Create gave it.
func TestEditChildGetsThePhysicalPath(t *testing.T) {
	w := newWorld(t)
	real, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	w.scenario[adapter.Codex] = `{"events":[{"type":"item.completed","item":{"type":"agent_message","text":"done"}}]}`
	b := w.borrower()
	b.Worktrees = linkedWorktrees{link: link}
	req := w.req(adapter.Edit, "add a helper")
	req.Vendor = "codex"
	if _, err := b.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	rec := w.records()
	if len(rec) != 1 {
		t.Fatalf("%d children", len(rec))
	}
	args := strings.Join(rec[0].Args, " ")
	if !strings.Contains(args, "-C "+real+string(filepath.Separator)) || strings.Contains(args, link) {
		t.Errorf("the child's command names the link, or not the real path:\n%s", args)
	}
	if !strings.Contains(args, `projects={"`+real+string(filepath.Separator)) {
		t.Errorf("the trust key is not the real path:\n%s", args)
	}
}
