-- aggy index schema, version 14: aggy gc (M3).
-- worktree_removed_at is when aggy removed an edit child's worktree and
-- branch (aggy reject or aggy gc); NULL while they may still exist. It is
-- set before the removal, as a claim, and cleared again if it fails.
-- reject removed both before this version, so its rows are marked.
-- worktree_accepted_at is when the person accepted the edit: aggy gc
-- keeps an accepted worktree and its branch (for aggy pr) for a while
-- after that.
ALTER TABLE children ADD COLUMN worktree_removed_at INTEGER;
ALTER TABLE children ADD COLUMN worktree_accepted_at INTEGER;
UPDATE children SET worktree_removed_at = last_active
    WHERE worktree <> '' AND worktree_state = 'rejected';
