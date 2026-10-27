-- aggy index schema, version 1.
-- Times are Unix milliseconds (UTC). Lists are JSON text.
-- This database holds ids, paths, and status. It never holds vendor tokens
-- or credentials.

-- An aggy session: the parent the person resumes.
CREATE TABLE parents (
    id          TEXT PRIMARY KEY,
    title       TEXT NOT NULL DEFAULT '',
    dir         TEXT NOT NULL,
    last_vendor TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    last_active INTEGER NOT NULL
);
CREATE INDEX parents_last_active ON parents (last_active);

-- A real vendor session inside a parent: interactive sessions and borrows.
CREATE TABLE children (
    id                TEXT PRIMARY KEY,
    parent_id         TEXT NOT NULL REFERENCES parents (id) ON DELETE CASCADE,
    vendor            TEXT NOT NULL,
    vendor_session_id TEXT NOT NULL DEFAULT '',  -- the vendor's conversation id
    kind              TEXT NOT NULL DEFAULT '',  -- '' for interactive, else the borrow kind
    title             TEXT NOT NULL DEFAULT '',
    model             TEXT NOT NULL DEFAULT '',  -- '' means the vendor's default
    cwd               TEXT NOT NULL,
    worktree          TEXT NOT NULL DEFAULT '',
    transcript_path   TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'open',  -- open | running | closed | gone
    lease_pid         INTEGER,                       -- process holding this session, if any
    lease_at          INTEGER,
    created_at        INTEGER NOT NULL,
    last_active       INTEGER NOT NULL
);
CREATE INDEX children_parent ON children (parent_id, last_active);
CREATE UNIQUE INDEX children_vendor_session
    ON children (vendor, vendor_session_id) WHERE vendor_session_id <> '';

-- One bounded headless task. Kept for stats after its child is pruned.
CREATE TABLE borrows (
    id          TEXT PRIMARY KEY,
    child_id    TEXT REFERENCES children (id) ON DELETE SET NULL,
    kind        TEXT NOT NULL,
    vendor      TEXT NOT NULL,
    model       TEXT NOT NULL DEFAULT '',
    task_hash   TEXT NOT NULL DEFAULT '',
    budget      TEXT NOT NULL DEFAULT '{}',  -- JSON: {"minutes":10,"turns":30}
    status      TEXT NOT NULL DEFAULT 'running',  -- running | done | failed | killed
    outcome     TEXT NOT NULL DEFAULT '',  -- error class or person's verdict
    artifacts   TEXT NOT NULL DEFAULT '[]',  -- JSON list of paths
    image_count INTEGER NOT NULL DEFAULT 0,
    tokens      INTEGER NOT NULL DEFAULT 0,
    report_path TEXT NOT NULL DEFAULT '',
    started_at  INTEGER NOT NULL,
    ended_at    INTEGER
);
CREATE INDEX borrows_kind ON borrows (kind, vendor, started_at);

-- The config-change manifest that aggy unwire reads (patch.Record).
CREATE TABLE changes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    file        TEXT NOT NULL,
    hash_before TEXT NOT NULL,  -- 'none' when the change created the file
    hash_after  TEXT NOT NULL,
    backup_path TEXT NOT NULL DEFAULT '',
    applied_at  INTEGER NOT NULL,
    undone_at   INTEGER
);
CREATE INDEX changes_file ON changes (file);

-- CLI probes: connection status only, never tokens.
CREATE TABLE probes (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    vendor       TEXT NOT NULL,
    cli_path     TEXT NOT NULL DEFAULT '',
    version      TEXT NOT NULL DEFAULT '',
    connected    INTEGER NOT NULL DEFAULT 0 CHECK (connected IN (0, 1)),
    capabilities TEXT NOT NULL DEFAULT '[]',  -- JSON list
    probed_at    INTEGER NOT NULL
);
CREATE INDEX probes_vendor ON probes (vendor, probed_at);

-- Append-only log behind aggy log. No foreign keys, so the log outlives
-- the rows it mentions.
CREATE TABLE events (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    at        INTEGER NOT NULL,
    parent_id TEXT NOT NULL DEFAULT '',
    child_id  TEXT NOT NULL DEFAULT '',
    type      TEXT NOT NULL,
    detail    TEXT NOT NULL DEFAULT ''  -- JSON
);
CREATE INDEX events_at ON events (at);

CREATE TRIGGER events_no_update BEFORE UPDATE ON events
BEGIN
    SELECT RAISE(ABORT, 'events is append-only');
END;

CREATE TRIGGER events_no_delete BEFORE DELETE ON events
BEGIN
    SELECT RAISE(ABORT, 'events is append-only');
END;
