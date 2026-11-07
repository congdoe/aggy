-- aggy index schema, version 21: OpenCode is a vendor setup can record (T6932).
-- SQLite cannot change a CHECK in place, so the table is rebuilt.
CREATE TABLE vendors_new (
    vendor     TEXT PRIMARY KEY CHECK (vendor IN ('claude', 'codex', 'grok', 'opencode')),
    connected  TEXT NOT NULL
               CHECK (connected IN ('connected', 'not_connected', 'unknown')),
    version    TEXT NOT NULL DEFAULT '',  -- '' when not installed or unreadable
    checked_at INTEGER NOT NULL
);
INSERT INTO vendors_new (vendor, connected, version, checked_at)
    SELECT vendor, connected, version, checked_at FROM vendors;
DROP TABLE vendors;
ALTER TABLE vendors_new RENAME TO vendors;
