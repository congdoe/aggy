-- aggy index schema, version 18: the skill folder aggy tool on linked
-- (L78-4). One row per tool whose skill is linked: the sha256 over the
-- folder's file names and contents when it was first linked. A later
-- link of a folder that hashes otherwise is refused, and a launch wires
-- none of that tool's MCP servers or hooks (R80-3). A link already made
-- is a symlink, so an edit after linking still reaches the CLIs; turning
-- the tool off everywhere unlinks it and clears the row.
CREATE TABLE tool_skill (
    tool      TEXT PRIMARY KEY,
    digest    TEXT NOT NULL,
    linked_at INTEGER NOT NULL
);
