-- aggy index schema, version 4: a process lease on a child also holds a
-- nonce. The holder keeps an flock on run/leases/<nonce>.lock for as long
-- as it lives, so a lease whose lock is free is stale even when its pid
-- has been reused by another process.
ALTER TABLE children ADD COLUMN lease_nonce TEXT;
