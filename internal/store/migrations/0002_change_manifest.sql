-- aggy index schema, version 2: the change manifest records what an exact
-- undo needs. SQLite cannot relax applied_at's NOT NULL in place, so the
-- table is rebuilt and its rows copied.

CREATE TABLE changes_v2 (
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
    undone_at    INTEGER
);

INSERT INTO changes_v2 (id, change_id, file, hash_before, hash_after, backup_path,
                        created_file, status, planned_at, applied_at, undone_at)
SELECT id, 'v1-' || id, file, hash_before, hash_after, backup_path,
       hash_before = 'none',
       CASE WHEN undone_at IS NULL THEN 'applied' ELSE 'undone' END,
       applied_at, applied_at, undone_at
FROM changes;

DROP TABLE changes;
ALTER TABLE changes_v2 RENAME TO changes;
CREATE INDEX changes_file ON changes (file);
CREATE INDEX changes_run ON changes (run_id, id);
