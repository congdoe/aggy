package borrow

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

// Worktrees makes and removes an edit child's own git worktree and
// branch. internal/cli implements it with git; this package only orders
// the calls. A worktree is git isolation, not a sandbox: each CLI's own
// sandbox stays on (design section 2).
type Worktrees interface {
	// Create makes a worktree and branch for borrow id from the main
	// checkout at repo, ready for the child, and returns it pending.
	Create(ctx context.Context, repo, id string) (store.Worktree, error)
	// Remove removes a worktree Create made, and its branch.
	Remove(ctx context.Context, wt store.Worktree) error
	// Tree stages everything in wt in its own index and returns the git
	// tree, which aggy accept compares against so a process the child
	// left behind cannot change the diff after the borrow ended.
	Tree(ctx context.Context, wt store.Worktree) (string, error)
}

// editGate refuses an edit on a vendor whose edit permission mode and
// sandbox are not recorded in docs/adapters.md and docs/verification.md.
//
//   - Codex: codex.EditLockdown: --sandbox workspace-write (confirmed live,
//     verification.md "Codex resume with sandbox") with the user's
//     config.toml ignored, so no MCP server, writable_roots, or
//     network_access from it widens the sandbox; approval stays at exec's
//     default.
//   - Claude: --permission-mode acceptEdits is unconfirmed, and nothing
//     recorded keeps its writes in the worktree. TODO(T6687): a live
//     check of acceptEdits under -p.
//   - Grok: write and search_replace take any absolute path, and no
//     sandbox is recorded for a writing child (--sandbox strict, T6740,
//     covers read-only kinds). TODO(T6687): a live check of a write
//     under --sandbox strict.
func editGate(v adapter.Vendor) error {
	if v == adapter.Codex {
		return nil
	}
	return fmt.Errorf("%s: edit: %w (its write permission mode and sandbox are unverified, so aggy does not let it write)", v, adapter.ErrUnsupported)
}

// editPaths returns req's paths relative to the repo root, as the child
// sees them in its worktree: an absolute path in the main checkout would
// point it at the one place it must not write.
func editPaths(req Request) ([]string, error) {
	var out []string
	for _, p := range req.Paths {
		if !within(p, req.Repo) {
			return nil, fmt.Errorf("path %s is outside the repo %s", p, req.Repo)
		}
		rel, err := filepath.Rel(req.Repo, p)
		if err != nil {
			return nil, err
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out, nil
}
