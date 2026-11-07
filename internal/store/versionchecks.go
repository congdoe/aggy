package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// VersionCheck is a row of version_checks: the result of one check of a CLI
// version newer than the adapter's tested range.
type VersionCheck struct {
	Vendor  string
	Version string
	Check   string
	// Stamp names what was checked against; a row with another stamp no
	// longer counts.
	Stamp  string
	Passed bool
	// Reason is why it failed, in aggy's words.
	Reason string
	At     time.Time
}

// PutVersionCheck records c, replacing that check's earlier result.
func (s *Store) PutVersionCheck(ctx context.Context, c VersionCheck) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO version_checks (vendor, version, check_name, stamp, passed, reason, checked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (vendor, version, check_name) DO UPDATE SET
		   stamp = excluded.stamp, passed = excluded.passed, reason = excluded.reason, checked_at = excluded.checked_at`,
		c.Vendor, c.Version, c.Check, c.Stamp, c.Passed, c.Reason, millis(c.At))
	return err
}

// GetVersionCheck returns the recorded result of a check made with stamp,
// or ErrNotFound when there is none.
func (s *Store) GetVersionCheck(ctx context.Context, vendor, version, check, stamp string) (VersionCheck, error) {
	c := VersionCheck{Vendor: vendor, Version: version, Check: check, Stamp: stamp}
	var at int64
	err := s.db.QueryRowContext(ctx,
		`SELECT passed, reason, checked_at FROM version_checks
		 WHERE vendor = ? AND version = ? AND check_name = ? AND stamp = ?`, vendor, version, check, stamp).
		Scan(&c.Passed, &c.Reason, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return VersionCheck{}, ErrNotFound
	}
	if err != nil {
		return VersionCheck{}, err
	}
	c.At = fromMillis(at)
	return c, nil
}
