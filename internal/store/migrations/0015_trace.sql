-- aggy index schema, version 15: diagnostic traces (T6753).
-- trace_id is the diagnostic log's trace (docs/logging.md) of the flow
-- that made the borrow or child, set when the row is made; NULL for rows
-- made before this version or with no diagnostic log. aggy accept, check,
-- and gc read it to join their records to that flow.
ALTER TABLE borrows ADD COLUMN trace_id TEXT;
ALTER TABLE children ADD COLUMN trace_id TEXT;
