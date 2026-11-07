-- aggy index schema, version 20: checks of a CLI newer than the tested range (T6929).
-- One row per vendor, CLI version, and check (help, features, selftest),
-- so a check runs once for each version. stamp names what aggy checked
-- against (its lockdown argv, or the features it reviewed): a row with
-- another stamp is ignored, so an aggy release checks again. reason holds
-- aggy's own words and a failing check's feature names, never the child's
-- output.
CREATE TABLE version_checks (
	vendor     TEXT NOT NULL,
	version    TEXT NOT NULL,
	check_name TEXT NOT NULL,
	stamp      TEXT NOT NULL,
	passed     INTEGER NOT NULL,
	reason     TEXT NOT NULL,
	checked_at INTEGER NOT NULL,
	PRIMARY KEY (vendor, version, check_name)
);
