-- aggy index schema, version 25: a change may name Claude's own trust key for
-- one folder as its reverse (T6977), 'claude-trust:<before>:<folder>'. Such a
-- change keeps no backup of the file (Claude's .claude.json holds its history
-- and account), so aggy unwire always puts back that one key as it was and
-- leaves the rest as Claude has written it since. SQLite cannot change a
-- CHECK in place, so the table is rebuilt with every column it has.
CREATE TABLE changes_v25 (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    change_id    TEXT NOT NULL UNIQUE,         -- names the backup folder backups/<change_id>
    run_id       TEXT NOT NULL DEFAULT '',     -- the wire run or group the change belongs to
    file         TEXT NOT NULL,
    hash_before  TEXT NOT NULL,                -- 'none' when the change creates the file
    hash_after   TEXT NOT NULL,
    backup_path  TEXT NOT NULL DEFAULT '',
    created_file INTEGER NOT NULL DEFAULT 0 CHECK (created_file IN (0, 1)),
    file_mode    INTEGER NOT NULL DEFAULT 0,   -- permission bits before, or of the created file
    created_dirs TEXT NOT NULL DEFAULT '[]',   -- JSON list of directories made, outermost first
    -- pending: recorded before the write, so a crash mid-change still
    -- leaves the expected hashes for unwire to compare against.
    status       TEXT NOT NULL DEFAULT 'applied'
                 CHECK (status IN ('pending', 'applied', 'noop', 'failed', 'undone')),
    planned_at   INTEGER NOT NULL,
    applied_at   INTEGER,
    undone_at    INTEGER,
    kind         TEXT NOT NULL DEFAULT 'change' CHECK (kind IN ('change', 'marker')),
    unlocked     INTEGER NOT NULL DEFAULT 1 CHECK (unlocked IN (0, 1)),
    reverse      TEXT NOT NULL DEFAULT ''
                 CHECK (reverse IN ('', 'aggy-hooks') OR reverse LIKE 'claude-trust:%')
);
INSERT INTO changes_v25 (id, change_id, run_id, file, hash_before, hash_after, backup_path,
                         created_file, file_mode, created_dirs, status, planned_at,
                         applied_at, undone_at, kind, unlocked, reverse)
    SELECT id, change_id, run_id, file, hash_before, hash_after, backup_path,
           created_file, file_mode, created_dirs, status, planned_at,
           applied_at, undone_at, kind, unlocked, reverse
    FROM changes;
DROP TABLE changes;
ALTER TABLE changes_v25 RENAME TO changes;
CREATE INDEX changes_file ON changes (file);
CREATE INDEX changes_run ON changes (run_id, id);
