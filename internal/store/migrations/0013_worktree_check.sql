-- aggy index schema, version 13: the check gate on an edit's diff (M3).
-- worktree_check is what the repo's checks (aggy check, from the main
-- checkout's config) said about the recorded tree: '' not run yet, pass,
-- fail, or skipped (the person declined to run them).
ALTER TABLE children ADD COLUMN worktree_check TEXT NOT NULL DEFAULT ''
    CHECK (worktree_check IN ('', 'pass', 'fail', 'skipped'));
