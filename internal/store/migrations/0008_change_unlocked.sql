-- aggy index schema, version 8: a change row says whether it was planned
-- without the per-target lock (a file system without flock). aggy unwire
-- trusts a newer change's plan as proof that an older pending change had
-- not written only when that plan held the lock. Rows from before this
-- version did not record it, so they default to unlocked and prove
-- nothing; InsertChange always writes the value explicitly.
ALTER TABLE changes ADD COLUMN unlocked INTEGER NOT NULL DEFAULT 1
    CHECK (unlocked IN (0, 1));
