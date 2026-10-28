-- aggy index schema, version 7: borrows run (M1). A running borrow holds
-- a lease like a child's (pid, nonce, and an flock on
-- run/leases/<nonce>.lock), so the concurrency cap counts only borrows
-- whose holder is alive, across processes. read_at marks a finished
-- report as read in aggy inbox.
ALTER TABLE borrows ADD COLUMN lease_pid INTEGER;
ALTER TABLE borrows ADD COLUMN lease_nonce TEXT;
ALTER TABLE borrows ADD COLUMN read_at INTEGER;
CREATE INDEX borrows_status ON borrows (status, vendor);
CREATE INDEX borrows_inbox ON borrows (report_path, read_at, started_at);
