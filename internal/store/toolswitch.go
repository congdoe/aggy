package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ToolSwitch is a row of tool_switches: one aggy tool on or off setting.
type ToolSwitch struct {
	Tool   string
	Vendor string // "" for every vendor
	Repo   string // "" for every repo, else the main checkout's canonical path
	On     bool
	// Digest is the sha256 of the definition turned on ("" for off).
	Digest string
	// ProxyDigest records the person's typed confirmation of the tool's
	// proxy for Vendor, which is never "" then (T6780):
	// tooldef.ProxyDigest of what they confirmed. Without it, or once the
	// definition no longer hashes to it, the proxy stays off, whatever On
	// says.
	ProxyDigest string
	At          time.Time
}

// toolProxySchema is the index version with tool_switches.proxy_digest.
const toolProxySchema = 19

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
		`INSERT INTO tool_switches (tool, vendor, repo, on_, digest, proxy_digest, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sw.Tool, sw.Vendor, sw.Repo, sw.On, sw.Digest, proxyDigest(sw), millis(sw.At)); err != nil {
		return err
	}
	return tx.Commit()
}

// proxyDigest is sw's confirmation, kept only on an "on" for one vendor.
func proxyDigest(sw ToolSwitch) string {
	if !sw.On || sw.Vendor == "" {
		return ""
	}
	return sw.ProxyDigest
}

// DeleteToolSwitches removes every setting of tool, and the digest aggy
// tool add recorded for it, as when its user definition is added anew
// or removed.
func (s *Store) DeleteToolSwitches(ctx context.Context, tool string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tool_switches WHERE tool = ?`, tool)
	if err == nil {
		_, err = s.db.ExecContext(ctx, `DELETE FROM tool_added WHERE tool = ?`, tool)
	}
	return err
}

// PutToolAdded records the digest of the definition aggy tool add showed
// and copied for tool.
func (s *Store) PutToolAdded(ctx context.Context, tool, digest string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tool_added (tool, digest, added_at) VALUES (?, ?, ?)
		 ON CONFLICT (tool) DO UPDATE SET digest = excluded.digest, added_at = excluded.added_at`,
		tool, digest, millis(at))
	return err
}

// ToolAdded returns the digest aggy tool add recorded for tool, and
// whether there is one.
func (s *Store) ToolAdded(ctx context.Context, tool string) (string, bool, error) {
	var d string
	err := s.db.QueryRowContext(ctx, `SELECT digest FROM tool_added WHERE tool = ?`, tool).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return d, err == nil, err
}

// ToolSwitches returns every setting, by tool, repo, and vendor. An
// index opened read-only before version 19 has no proxy column: every
// proxy is off there.
func (s *Store) ToolSwitches(ctx context.Context) ([]ToolSwitch, error) {
	v, err := s.Version(ctx)
	if err != nil {
		return nil, err
	}
	proxy := "''"
	if v >= toolProxySchema {
		proxy = "proxy_digest"
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT tool, vendor, repo, on_, digest, `+proxy+`, updated_at FROM tool_switches ORDER BY tool, repo, vendor`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ToolSwitch
	for rows.Next() {
		var sw ToolSwitch
		var at int64
		if err := rows.Scan(&sw.Tool, &sw.Vendor, &sw.Repo, &sw.On, &sw.Digest, &sw.ProxyDigest, &at); err != nil {
			return nil, err
		}
		sw.At = fromMillis(at)
		out = append(out, sw)
	}
	return out, rows.Err()
}

// ToolSkill returns the digest of tool's skill folder recorded when it
// was first linked (L78-4), and whether there is one.
func (s *Store) ToolSkill(ctx context.Context, tool string) (string, bool, error) {
	var d string
	err := s.db.QueryRowContext(ctx, `SELECT digest FROM tool_skill WHERE tool = ?`, tool).Scan(&d)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return d, err == nil, err
}

// PutToolSkill records the digest of tool's skill folder as linked.
func (s *Store) PutToolSkill(ctx context.Context, tool, digest string, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO tool_skill (tool, digest, linked_at) VALUES (?, ?, ?)
		 ON CONFLICT (tool) DO UPDATE SET digest = excluded.digest, linked_at = excluded.linked_at`,
		tool, digest, millis(at))
	return err
}

// DeleteToolSkill forgets tool's skill folder digest, once no vendor
// links it.
func (s *Store) DeleteToolSkill(ctx context.Context, tool string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM tool_skill WHERE tool = ?`, tool)
	return err
}
