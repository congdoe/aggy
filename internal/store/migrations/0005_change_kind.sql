-- aggy index schema, version 5: a change row says what it is. A write run
-- that changes nothing records one marker row, so the next setup knows a
-- setup has written; it names no file and has nothing to undo. Giving it
-- its own kind lets aggy unwire and ChangesNewestFirst skip it by kind
-- rather than by guessing from an empty file name.
ALTER TABLE changes ADD COLUMN kind TEXT NOT NULL DEFAULT 'change'
    CHECK (kind IN ('change', 'marker'));

-- Markers written before version 5: a no-op row with no file.
UPDATE changes SET kind = 'marker' WHERE file = '' AND status = 'noop';
