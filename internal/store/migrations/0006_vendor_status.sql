-- aggy index schema, version 6: what aggy setup last found for each
-- vendor, one row per vendor. It holds exactly whether the CLI reports
-- itself signed in, its version, and when that was checked: never tokens,
-- paths, or account details. A vendor setup skipped keeps its old row,
-- or has none.
CREATE TABLE vendors (
    vendor     TEXT PRIMARY KEY CHECK (vendor IN ('claude', 'codex', 'grok')),
    connected  TEXT NOT NULL
               CHECK (connected IN ('connected', 'not_connected', 'unknown')),
    version    TEXT NOT NULL DEFAULT '',  -- '' when not installed or unreadable
    checked_at INTEGER NOT NULL
);
