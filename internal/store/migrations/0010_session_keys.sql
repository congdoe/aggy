-- aggy index schema, version 10: sessions (M2).
-- dir_key is the parent directory's canonical form (safefs.CanonicalPath:
-- symlinks resolved, case-folded where the file system ignores case), so
-- two spellings of one directory find the same parent. dir_id is the
-- directory's device and inode at creation, so a directory removed and
-- made again (a re-clone) is not taken for the old one. Rows from before
-- this version have neither and match by dir alone.
ALTER TABLE parents ADD COLUMN dir_key TEXT NOT NULL DEFAULT '';
ALTER TABLE parents ADD COLUMN dir_id TEXT NOT NULL DEFAULT '';
CREATE INDEX parents_dir_key ON parents (dir_key, last_active);
-- vendor_home is the vendor's home folder (CLAUDE_CONFIG_DIR, CODEX_HOME,
-- GROK_HOME, or ~/.<vendor>) when the child was launched, where its
-- session files are looked for; '' means the current environment's.
ALTER TABLE children ADD COLUMN vendor_home TEXT NOT NULL DEFAULT '';
