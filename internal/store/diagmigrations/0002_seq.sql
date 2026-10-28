-- aggy diagnostic log schema, version 2: a stable sequence (T6753).
-- seq is an INTEGER PRIMARY KEY with AUTOINCREMENT: the order records
-- were written, which VACUUM never renumbers and a prune never reuses,
-- so aggy diag --follow can resume after the last seq it printed. Each
-- record keeps its rowid as its seq, so a follow that began on version
-- 1 goes on where it was.
CREATE TABLE records_v2 (
    seq               INTEGER PRIMARY KEY AUTOINCREMENT,
    id                TEXT NOT NULL UNIQUE,       -- ULID
    schema            INTEGER NOT NULL,
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
INSERT INTO records_v2 (seq, id, schema, ts, level, component, event, code, msg, attrs, duration_ms, exit_code, signal,
    aggy_version, os, run_id, trace_id, cause_id, parent_id, child_id, borrow_id, worktree_id, vendor, vendor_session_id)
SELECT rowid, id, schema, ts, level, component, event, code, msg, attrs, duration_ms, exit_code, signal,
    aggy_version, os, run_id, trace_id, cause_id, parent_id, child_id, borrow_id, worktree_id, vendor, vendor_session_id
FROM records ORDER BY rowid;
DROP TABLE records;
ALTER TABLE records_v2 RENAME TO records;
CREATE INDEX records_ts ON records (ts);
CREATE INDEX records_trace ON records (trace_id);
CREATE INDEX records_run ON records (run_id);
CREATE INDEX records_borrow ON records (borrow_id);
CREATE INDEX records_child ON records (child_id);
CREATE INDEX records_code ON records (code);
