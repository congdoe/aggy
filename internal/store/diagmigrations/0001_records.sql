-- aggy diagnostic log (diag.db) schema, version 1. docs/logging.md is the
-- convention. Times are Unix milliseconds (UTC); attrs is a JSON object.
-- This database holds ids, codes, aggy-state paths, exit codes, and
-- durations. It never holds prompt or task text, file contents, diffs,
-- model output, environment values, credentials, or vendor transcripts.
-- Entity ids (parent, child, borrow, worktree) live here, not in the
-- index, so the index needs no migration for them.
CREATE TABLE records (
    id                TEXT PRIMARY KEY,           -- ULID
    schema            INTEGER NOT NULL,           -- the record schema version
    ts                INTEGER NOT NULL,
    level             TEXT NOT NULL CHECK (level IN ('error', 'warn', 'info', 'debug')),
    component         TEXT NOT NULL,
    event             TEXT NOT NULL,
    code              TEXT NOT NULL DEFAULT '',
    msg               TEXT NOT NULL DEFAULT '',
    attrs             TEXT NOT NULL DEFAULT '{}',
    duration_ms       INTEGER,
    exit_code         INTEGER,
    signal            TEXT NOT NULL DEFAULT '',
    aggy_version      TEXT NOT NULL DEFAULT '',
    os                TEXT NOT NULL DEFAULT '',
    run_id            TEXT NOT NULL,
    trace_id          TEXT NOT NULL DEFAULT '',
    cause_id          TEXT NOT NULL DEFAULT '',
    parent_id         TEXT NOT NULL DEFAULT '',
    child_id          TEXT NOT NULL DEFAULT '',
    borrow_id         TEXT NOT NULL DEFAULT '',
    worktree_id       TEXT NOT NULL DEFAULT '',
    vendor            TEXT NOT NULL DEFAULT '',
    vendor_session_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX records_ts ON records (ts);
CREATE INDEX records_trace ON records (trace_id);
CREATE INDEX records_run ON records (run_id);
CREATE INDEX records_borrow ON records (borrow_id);
CREATE INDEX records_child ON records (child_id);
CREATE INDEX records_code ON records (code);
