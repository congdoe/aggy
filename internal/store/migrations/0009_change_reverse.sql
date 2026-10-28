-- aggy index schema, version 9: a change row may name a structural
-- reverse. aggy unwire restores a change's old bytes only while the file
-- still holds what aggy wrote; once the file has changed since (the
-- person, or the vendor CLI rewriting its own settings), a change with a
-- reverse is undone by taking aggy's own entries out of the file as it is
-- now, leaving everything else. 'aggy-hooks' removes aggy's hook entries
-- from a Claude-format hooks file. '' means byte restore only.
ALTER TABLE changes ADD COLUMN reverse TEXT NOT NULL DEFAULT ''
    CHECK (reverse IN ('', 'aggy-hooks'));
