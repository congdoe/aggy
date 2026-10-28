package store

import (
	"context"
	"time"
)

// VendorStatus is a row of vendors: what aggy setup last found for one
// vendor's CLI. Sign-in status and version only, never tokens.
type VendorStatus struct {
	Vendor    string    // claude, codex, or grok
	Connected string    // connected, not_connected, or unknown
	Version   string    // "" when the CLI is missing or its version unreadable
	CheckedAt time.Time // when setup checked
}

// PutVendorStatus records v, replacing the vendor's earlier row.
func (s *Store) PutVendorStatus(ctx context.Context, v VendorStatus) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO vendors (vendor, connected, version, checked_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (vendor) DO UPDATE SET connected = excluded.connected,
		     version = excluded.version, checked_at = excluded.checked_at`,
		v.Vendor, v.Connected, v.Version, millis(v.CheckedAt))
	return err
}

// VendorStatuses returns every recorded vendor, by name.
func (s *Store) VendorStatuses(ctx context.Context) ([]VendorStatus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT vendor, connected, version, checked_at FROM vendors ORDER BY vendor`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []VendorStatus
	for rows.Next() {
		var v VendorStatus
		var at int64
		if err := rows.Scan(&v.Vendor, &v.Connected, &v.Version, &at); err != nil {
			return nil, err
		}
		v.CheckedAt = fromMillis(at)
		out = append(out, v)
	}
	return out, rows.Err()
}
