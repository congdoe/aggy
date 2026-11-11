-- aggy index schema, version 29: aggy key (T6931). The index keeps a key's
-- name and the OpenCode provider it is bound to, never its value: the value
-- is in the OS keychain (the macOS Keychain or the Linux secret service),
-- under the service aggy-key with the name as the account.
CREATE TABLE keys (
    name     TEXT PRIMARY KEY,         -- the person's name for the key
    provider TEXT NOT NULL UNIQUE,     -- the OpenCode provider it is for, one key each
    added_at INTEGER NOT NULL          -- Unix milliseconds
);
