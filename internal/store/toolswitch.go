package store

import (
	"context"
	"time"
)

// ToolSwitch is a row of tool_switches: one aggy tool on or off setting.
type ToolSwitch struct {
	Tool   string
	Vendor string // "" for every vendor
	Repo   string // "" for every repo, else the repo's real path
	On     bool
	At     time.Time
}

// PutToolSwitch records sw. It replaces every setting of the tool that
// sw covers (the same or a narrower vendor and repo), so the newest
// setting holds wherever it applies: aggy tool off demo turns demo off in
// every repo, including one where it was turned on before.
func (s *Store) PutToolSwitch(ctx context.Context, sw ToolSwitch) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM tool_switches WHERE tool = ? AND (? = '' OR vendor = ?) AND (? = '' OR repo = ?)`,
		sw.Tool, sw.Vendor, sw.Vendor, sw.Repo, sw.Repo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tool_switches (tool, vendor, repo, on_, updated_at) VALUES (?, ?, ?, ?, ?)`,
		sw.Tool, sw.Vendor, sw.Repo, sw.On, millis(sw.At)); err != nil {
		return err
	}
	return tx.Commit()
}

// ToolSwitches returns every setting, by tool, repo, and vendor.
func (s *Store) ToolSwitches(ctx context.Context) ([]ToolSwitch, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT tool, vendor, repo, on_, updated_at FROM tool_switches ORDER BY tool, repo, vendor`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ToolSwitch
	for rows.Next() {
		var sw ToolSwitch
		var at int64
		if err := rows.Scan(&sw.Tool, &sw.Vendor, &sw.Repo, &sw.On, &at); err != nil {
			return nil, err
		}
		sw.At = fromMillis(at)
		out = append(out, sw)
	}
	return out, rows.Err()
}
