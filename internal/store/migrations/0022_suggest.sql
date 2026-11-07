-- aggy index schema, version 22: command suggestions from Claude Code's
-- session files (T6940, docs/design.md, section 15). suggest_files holds each
-- file's read position with what a scan keeps between lines, so a second aggy
-- cannot count a line twice. suggest_commands holds counts of plain commands,
-- by the main checkout's canonical path. A row here is a candidate and never an
-- allow: only a person at the keyboard adds one. A command with anything
-- secret-shaped in it is dropped whole before it gets here. suggest_status
-- holds what the last scans saw, for aggy doctor.
CREATE TABLE suggest_files (
	path       TEXT PRIMARY KEY,
	offset     INTEGER NOT NULL,
	mode       TEXT NOT NULL,
	pending    TEXT NOT NULL,
	shells     INTEGER NOT NULL,
	with_mode  INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE TABLE suggest_commands (
	repo       TEXT NOT NULL,
	command    TEXT NOT NULL,
	count      INTEGER NOT NULL,
	announced  INTEGER NOT NULL DEFAULT 0,
	dismissed  INTEGER NOT NULL DEFAULT 0,
	first_seen INTEGER NOT NULL,
	last_seen  INTEGER NOT NULL,
	PRIMARY KEY (repo, command)
);
