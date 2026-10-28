-- aggy index schema, version 17: the definition aggy tool add showed
-- (S63-16). One row per user tool added with aggy tool add: the sha256
-- of the file it previewed and copied. aggy tool on refuses a file whose
-- digest is no longer this one, so what is turned on is what was shown.
-- A definition put in ~/.aggy/tools by hand has no row.
CREATE TABLE tool_added (
    tool     TEXT PRIMARY KEY,
    digest   TEXT NOT NULL,
    added_at INTEGER NOT NULL
);
