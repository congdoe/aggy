-- aggy index schema, version 18: the skill folder aggy tool on linked
-- (L78-4). One row per tool whose skill is linked: the sha256 over the
-- folder's file names and contents when it was first linked. A later
-- link of a folder that hashes otherwise is refused, so a changed skill
-- never reaches a CLI unseen; turning the tool off everywhere clears it.
CREATE TABLE tool_skill (
    tool      TEXT PRIMARY KEY,
    digest    TEXT NOT NULL,
    linked_at INTEGER NOT NULL
);
