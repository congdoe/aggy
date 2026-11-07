package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SuggestFile is a row of suggest_files: how far one session file was read.
type SuggestFile struct {
	Path   string
	Offset int64
	Mode   string
	// Pending is suggest's calls waiting for a result, as JSON.
	Pending string
	// Shells and WithMode count shell calls seen, and those with a mode known.
	Shells, WithMode int64
}

// SuggestCount adds N runs of a command in a repo.
type SuggestCount struct {
	Repo, Command string
	N             int
}

// SuggestCandidate is a command that has run often enough to propose.
type SuggestCandidate struct {
	Repo, Command string
	Count         int
}

// maxSuggestPerRepo bounds suggest_commands per repo, so a forged file cannot
// grow the index without limit: past it, only known commands are counted.
const maxSuggestPerRepo = 2000

// SuggestFileState reads a file's row. Absent is the zero value at that path.
func (s *Store) SuggestFileState(ctx context.Context, path string) (SuggestFile, error) {
	f := SuggestFile{Path: path, Pending: "[]"}
	err := s.db.QueryRowContext(ctx, `SELECT offset, mode, pending, shells, with_mode FROM suggest_files WHERE path = ?`, path).
		Scan(&f.Offset, &f.Mode, &f.Pending, &f.Shells, &f.WithMode)
	if errors.Is(err, sql.ErrNoRows) {
		return f, nil
	}
	return f, err
}

// SaveSuggest records a stretch of a file and its counts in one transaction, if
// the file's position is still oldOffset. Otherwise another aggy read that
// stretch, so nothing is added and saved is false: no line counts twice.
func (s *Store) SaveSuggest(ctx context.Context, oldOffset int64, f SuggestFile, counts []SuggestCount, now time.Time) (saved bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var cur int64
	switch err := tx.QueryRowContext(ctx, `SELECT offset FROM suggest_files WHERE path = ?`, f.Path).Scan(&cur); {
	case errors.Is(err, sql.ErrNoRows):
		// A first read: cur stays 0.
	case err != nil:
		return false, err
	}
	if cur != oldOffset {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO suggest_files (path, offset, mode, pending, shells, with_mode, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (path) DO UPDATE SET offset = excluded.offset, mode = excluded.mode, pending = excluded.pending,
		 shells = excluded.shells, with_mode = excluded.with_mode, updated_at = excluded.updated_at`,
		f.Path, f.Offset, f.Mode, f.Pending, f.Shells, f.WithMode, millis(now)); err != nil {
		return false, err
	}
	for _, c := range counts {
		res, err := tx.ExecContext(ctx,
			`UPDATE suggest_commands SET count = count + ?, last_seen = ? WHERE repo = ? AND command = ?`,
			c.N, millis(now), c.Repo, c.Command)
		if err != nil {
			return false, err
		}
		if n, err := res.RowsAffected(); err != nil || n > 0 {
			if err != nil {
				return false, err
			}
			continue
		}
		var rows int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM suggest_commands WHERE repo = ?`, c.Repo).Scan(&rows); err != nil {
			return false, err
		}
		if rows >= maxSuggestPerRepo {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO suggest_commands (repo, command, count, first_seen, last_seen) VALUES (?, ?, ?, ?, ?)`,
			c.Repo, c.Command, c.N, millis(now), millis(now)); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// SuggestCandidates are the commands run at least min times and not turned
// down, for one repo, or every repo when repo is "", most run first.
func (s *Store) SuggestCandidates(ctx context.Context, repo string, min int) ([]SuggestCandidate, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT repo, command, count FROM suggest_commands
		 WHERE count >= ? AND dismissed = 0 AND (? = '' OR repo = ?)
		 ORDER BY count DESC, repo, command LIMIT 200`, min, repo, repo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SuggestCandidate
	for rows.Next() {
		var c SuggestCandidate
		if err := rows.Scan(&c.Repo, &c.Command, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// AnnounceSuggest returns how many candidates of repo have not been mentioned,
// and marks them, so the launch line shows only when there is something new.
func (s *Store) AnnounceSuggest(ctx context.Context, repo string, min int) (int, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE suggest_commands SET announced = 1 WHERE repo = ? AND count >= ? AND dismissed = 0 AND announced = 0`, repo, min)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// DismissSuggest records that the person turned a command down.
func (s *Store) DismissSuggest(ctx context.Context, repo, command string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE suggest_commands SET dismissed = 1 WHERE repo = ? AND command = ?`, repo, command)
	return err
}

// SuggestSeen totals what the scans saw: shell calls, and those with a mode.
func (s *Store) SuggestSeen(ctx context.Context) (shells, withMode int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(shells), 0), COALESCE(SUM(with_mode), 0) FROM suggest_files`).Scan(&shells, &withMode)
	return shells, withMode, err
}
