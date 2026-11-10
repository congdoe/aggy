-- aggy index schema, version 23: a launch's probe is looked up by the CLI
-- binary's identity (T6964): a hash of its resolved path, size, modification
-- time, device, and inode, so an unchanged binary starts no probe process and
-- a changed one is probed again. '' on a row a borrow's probe recorded.
ALTER TABLE probes ADD COLUMN binary_id TEXT NOT NULL DEFAULT '';
CREATE INDEX probes_binary ON probes (vendor, binary_id, probed_at);
