// Package store owns the SQLite index at ~/.aggy/aggy.db ($AGGY_HOME
// overrides the directory): parents, children, borrows, config changes,
// capability probes, and events. It uses the pure-Go modernc.org/sqlite
// driver, so aggy builds without cgo. Migrations are embedded SQL files
// tracked by PRAGMA user_version.
package store
