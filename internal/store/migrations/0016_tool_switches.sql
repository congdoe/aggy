-- aggy index schema, version 16: the tool switchboard (T6708).
-- One row per setting the person made with aggy tool on or off: a tool,
-- for one vendor or every vendor (vendor = ''), in one repo or every repo
-- (repo = '', else the canonical path of the repo's main checkout, so a
-- setting follows the repo into its worktrees). The most specific row
-- wins; a tool with no row takes its definition's default. digest is the
-- sha256 of the user definition that was turned on: a changed or
-- replaced file is off until turned on again. A repo's own config can
-- only turn tools off, and is never stored here.
--
-- Dev note (S63-15): digest was added to this file in place, before any
-- release. A dev index migrated to version 16 by a build from 202041f
-- has tool_switches without it, and aggy tool fails there with "no such
-- column: digest". Repair such an index by hand (no release has one):
--   sqlite3 ~/.aggy/aggy.db "ALTER TABLE tool_switches ADD COLUMN digest TEXT NOT NULL DEFAULT ''"
CREATE TABLE tool_switches (
    tool       TEXT NOT NULL,
    vendor     TEXT NOT NULL,
    repo       TEXT NOT NULL,
    on_        INTEGER NOT NULL CHECK (on_ IN (0, 1)),
    digest     TEXT NOT NULL DEFAULT '',
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (tool, vendor, repo)
);
