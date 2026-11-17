-- aggy index schema, version 31: whether an on-screen child ever had a turn
-- end that aggy counted (T6982). A Claude switch before the first turn end
-- goes in one step only in a folder where an earlier on-screen Claude session
-- ended a turn: Claude had shown its prompt there, so the folder-trust dialog
-- was answered then.
ALTER TABLE children ADD COLUMN turn_seen INTEGER NOT NULL DEFAULT 0;
