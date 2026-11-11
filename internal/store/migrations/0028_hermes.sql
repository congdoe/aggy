-- aggy index schema, version 28: Hermes Agent is a vendor setup can record,
-- and aggy keeps a row for each Hermes session on screen (T6933). Hermes runs
-- its hooks in its own daemon, with no aggy variable and the daemon's folder,
-- so aggy's hook finds the launch here: by the profile and the session id the
-- payload names, and the supervisor to tell by its socket name and token.
CREATE TABLE vendors_new (
    vendor     TEXT PRIMARY KEY CHECK (vendor IN ('claude', 'codex', 'grok', 'opencode', 'hermes')),
    connected  TEXT NOT NULL
               CHECK (connected IN ('connected', 'not_connected', 'unknown')),
    version    TEXT NOT NULL DEFAULT '',  -- '' when not installed or unreadable
    checked_at INTEGER NOT NULL
);
INSERT INTO vendors_new (vendor, connected, version, checked_at)
    SELECT vendor, connected, version, checked_at FROM vendors;
DROP TABLE vendors;
ALTER TABLE vendors_new RENAME TO vendors;

CREATE TABLE hermes_launches (
    id          INTEGER PRIMARY KEY,
    parent_id   TEXT NOT NULL,             -- the supervisor's socket: run/<parent_id>.sock
    child_id    TEXT NOT NULL,
    profile     TEXT NOT NULL,             -- aggy's Hermes profile for the repo
    root        TEXT NOT NULL,             -- the repo root, absolute
    cwd         TEXT NOT NULL,             -- the session's folder, absolute
    plan        TEXT NOT NULL DEFAULT '',  -- the decision plan's path; '' with --no-policy
    plan_sum    TEXT NOT NULL DEFAULT '',  -- its SHA-256, the pin the hook checks
    token       TEXT NOT NULL,             -- what the hook shows the supervisor
    session_id  TEXT NOT NULL DEFAULT '',  -- Hermes' session id, once a hook named it
    started_at  INTEGER NOT NULL
);
CREATE INDEX hermes_launches_profile ON hermes_launches (profile);

-- A change may name aggy's block in its Hermes profile's config.yaml as its
-- reverse ('hermes-block'): it keeps no backup, as the file may hold what the
-- person's root config seeded, and aggy unwire takes the block out.
CREATE TABLE changes_v28 (
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
                 CHECK (reverse IN ('', 'aggy-hooks', 'hermes-block') OR reverse LIKE 'claude-trust:%')
);
INSERT INTO changes_v28 (id, change_id, run_id, file, hash_before, hash_after, backup_path,
                         created_file, file_mode, created_dirs, status, planned_at,
                         applied_at, undone_at, kind, unlocked, reverse)
    SELECT id, change_id, run_id, file, hash_before, hash_after, backup_path,
           created_file, file_mode, created_dirs, status, planned_at,
           applied_at, undone_at, kind, unlocked, reverse
    FROM changes;
DROP TABLE changes;
ALTER TABLE changes_v28 RENAME TO changes;
CREATE INDEX changes_file ON changes (file);
CREATE INDEX changes_run ON changes (run_id, id);
