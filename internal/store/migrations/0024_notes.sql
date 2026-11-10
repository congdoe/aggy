-- aggy index schema, version 24: one-time notes and answers (T6966), such as
-- whether the first-run setup offer was taken or declined, so bare aggy asks
-- once and a declined offer's note is shown once, never on every launch.
CREATE TABLE notes (
    name  TEXT PRIMARY KEY,   -- a fixed name from internal/cli
    value TEXT NOT NULL,      -- a fixed word: accepted, declined
    at    INTEGER NOT NULL    -- when, in Unix milliseconds
);
