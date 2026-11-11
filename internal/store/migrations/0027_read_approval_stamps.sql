-- aggy index schema: more of a read pre-approval's file identity (T6976, CI
-- on ubuntu). Device and inode alone matched a replaced file once Linux gave
-- the freed inode number to the new one, so aggy permissions add also records
-- the file's size, modification time, and change time (ctime), all in
-- nanoseconds. A row written before this has them NULL: it matches nothing,
-- so the person is asked until they add the rule again.
ALTER TABLE read_approvals ADD COLUMN size INTEGER;
ALTER TABLE read_approvals ADD COLUMN mod_ns INTEGER;
ALTER TABLE read_approvals ADD COLUMN change_ns INTEGER;
