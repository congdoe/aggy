-- aggy index schema, version 11: whether a child's session file was ever
-- seen. A Claude session quit before its first message likely never gets
-- a file, and aggy's view of Claude's layout is unverified, so a miss is
-- only taken as "gone" for a child whose file was never seen (D23-7).
ALTER TABLE children ADD COLUMN file_seen INTEGER NOT NULL DEFAULT 0;
