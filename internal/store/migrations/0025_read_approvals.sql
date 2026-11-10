-- aggy index schema, version 25: the identity of each file an exact read
-- pre-approval names (T6976, PR #129 review), recorded by aggy permissions add
-- allow at a terminal, so the policy decision hook allows a read only of the
-- same file: a file replaced, or a link put in its place, asks again.
CREATE TABLE read_approvals (
    path     TEXT PRIMARY KEY,   -- the cleaned absolute path the rule names
    dev      INTEGER NOT NULL,   -- the file's device, as Lstat gave it
    ino      INTEGER NOT NULL,   -- the file's inode
    added_at INTEGER NOT NULL    -- when, in Unix milliseconds
);
