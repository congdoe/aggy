-- aggy index schema, version 16: the tool switchboard (T6708).
-- One row per setting the person made with aggy tool on or off: a tool,
-- for one vendor or every vendor (vendor = ''), in one repo or every repo
-- (repo = '', else the repo's real path). The most specific row wins; a
-- tool with no row takes its definition's default. A repo's own config
-- can only turn tools off, and is never stored here.
CREATE TABLE tool_switches (
    tool       TEXT NOT NULL,
    vendor     TEXT NOT NULL,
    repo       TEXT NOT NULL,
    on_        INTEGER NOT NULL CHECK (on_ IN (0, 1)),
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (tool, vendor, repo)
);
