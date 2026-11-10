package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Probe is a row of probes: what aggy learned about one installed CLI.
// Connection status only, never tokens.
type Probe struct {
	ID           int64
	Vendor       string
	CLIPath      string
	Version      string
	Connected    bool
	Capabilities []string
	ProbedAt     time.Time
	// BinaryID is the identity of the CLI binary a launch probed
	// (probe.BinaryID), or "" (T6964).
	BinaryID string
}

// InsertProbe records a probe.
func (s *Store) InsertProbe(ctx context.Context, p Probe) error {
	caps, err := json.Marshal(nonNil(p.Capabilities))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO probes (vendor, cli_path, version, connected, capabilities, probed_at, binary_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.Vendor, p.CLIPath, p.Version, p.Connected, string(caps), millis(p.ProbedAt), p.BinaryID)
	return err
}

// LatestProbe returns the newest probe of vendor's CLI at cliPath that
// reported version, or ErrNotFound.
func (s *Store) LatestProbe(ctx context.Context, vendor, cliPath, version string) (Probe, error) {
	return s.probe(ctx, `WHERE vendor = ? AND cli_path = ? AND version = ?`, vendor, cliPath, version)
}

// ProbeByBinary returns the newest probe of vendor's CLI recorded with
// binaryID, or ErrNotFound. An empty binaryID matches nothing.
func (s *Store) ProbeByBinary(ctx context.Context, vendor, binaryID string) (Probe, error) {
	if binaryID == "" {
		return Probe{}, ErrNotFound
	}
	return s.probe(ctx, `WHERE vendor = ? AND binary_id = ?`, vendor, binaryID)
}

// probe returns the newest probes row where says, or ErrNotFound.
func (s *Store) probe(ctx context.Context, where string, args ...any) (Probe, error) {
	var p Probe
	var caps string
	var at int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, vendor, cli_path, version, connected, capabilities, probed_at, binary_id FROM probes `+
			where+` ORDER BY probed_at DESC, id DESC LIMIT 1`, args...).
		Scan(&p.ID, &p.Vendor, &p.CLIPath, &p.Version, &p.Connected, &caps, &at, &p.BinaryID)
	if errors.Is(err, sql.ErrNoRows) {
		return Probe{}, ErrNotFound
	}
	if err != nil {
		return Probe{}, err
	}
	if err := json.Unmarshal([]byte(caps), &p.Capabilities); err != nil {
		return Probe{}, err
	}
	p.ProbedAt = fromMillis(at)
	return p, nil
}
