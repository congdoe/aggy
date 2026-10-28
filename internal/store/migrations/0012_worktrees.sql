-- aggy index schema, version 12: edit worktrees (M3).
-- An edit child works in its own git worktree under the state dir;
-- children.worktree (since version 1) is that path. branch is aggy's own
-- branch for it (aggy/edit-<borrow id>), worktree_base the commit it was
-- made from, worktree_repo the main checkout, worktree_git_dir its admin
-- dir in the repo's .git (aggy's git calls on the worktree name it, never
-- the .git file the child could rewrite), and worktree_state is pending
-- until the person accepts or rejects the diff.
ALTER TABLE children ADD COLUMN branch TEXT NOT NULL DEFAULT '';
ALTER TABLE children ADD COLUMN worktree_base TEXT NOT NULL DEFAULT '';
ALTER TABLE children ADD COLUMN worktree_repo TEXT NOT NULL DEFAULT '';
ALTER TABLE children ADD COLUMN worktree_git_dir TEXT NOT NULL DEFAULT '';
ALTER TABLE children ADD COLUMN worktree_state TEXT NOT NULL DEFAULT ''
    CHECK (worktree_state IN ('', 'pending', 'accepted', 'rejected'));
