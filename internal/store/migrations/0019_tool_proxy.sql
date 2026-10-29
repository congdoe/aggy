-- aggy index schema, version 19: a tool's local proxy (T6780). proxy is 1
-- only on a row aggy tool on wrote for one named vendor after the person
-- read the warning and typed the confirmation at a terminal. Every row
-- written before, or without that confirmation, holds 0, and the tool's
-- proxy stays off for it while its other parts follow on_.
ALTER TABLE tool_switches ADD COLUMN proxy INTEGER NOT NULL DEFAULT 0 CHECK (proxy IN (0, 1));
