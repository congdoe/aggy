package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// sessionFiles finds a vendor session's files in the vendor's sessions
// folder and returns when they last changed. It stats and lists folders
// only: it never reads a session file, never follows a symlink inside
// the sessions folder, and goes through safefs, which refuses the
// credential files. The folders:
//
//   - Claude: <$CLAUDE_CONFIG_DIR or ~/.claude>/projects/<one folder per
//     cwd>/<session id>.jsonl. Unverified: docs/verification.md does not
//     record this layout, so any project folder holding <id>.jsonl counts.
//   - Codex: <$CODEX_HOME or ~/.codex>/sessions/<YYYY>/<MM>/<DD>/
//     rollout-<timestamp>-<thread id>.jsonl, created at the first persist
//     (source-verified in the hooks work: codex-rs/rollout/src/recorder.rs
//     at 6aaac694). Which clock names the date folder is not recorded, so
//     the days around the child's start are searched in local time and UTC.
//     TODO(T6660): share codex.RolloutThreadID and RolloutDirs once the
//     hooks branch merges; internal/transcript (T6639) may also take this.
//   - Grok: <$GROK_HOME or ~/.grok>/sessions/<url-encoded cwd>/<session
//     id>/, a folder (docs/adapters.md, Grok); its newest entry counts.
//
// The zero value uses the environment.
type sessionFiles struct {
	getenv func(string) string // nil means os.Getenv

	claudeDirs []string // project folders, listed once
	listed     bool
}

// errNoSessionFile means the vendor has no file for the session.
var errNoSessionFile = errors.New("no session file")

func (f *sessionFiles) env(k string) string {
	if f.getenv != nil {
		return f.getenv(k)
	}
	return os.Getenv(k)
}

// root is the vendor's sessions folder: the relocation variable when
// set, else ~/.<vendor>, then projects (Claude) or sessions.
func (f *sessionFiles) root(v adapter.Vendor) (string, error) {
	relocate := map[adapter.Vendor]string{adapter.Claude: "CLAUDE_CONFIG_DIR", adapter.Codex: "CODEX_HOME", adapter.Grok: "GROK_HOME"}[v]
	sub := "sessions"
	if v == adapter.Claude {
		sub = "projects"
	}
	if d := f.env(relocate); d != "" {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("%s is not an absolute path", relocate)
		}
		return filepath.Join(filepath.Clean(d), sub), nil
	}
	h := f.env("HOME")
	if h == "" || !filepath.IsAbs(h) {
		return "", errors.New("no absolute HOME")
	}
	return filepath.Join(h, "."+string(v), sub), nil
}

// LastChanged returns when the session id of vendor v last changed on
// disk, or errNoSessionFile when the vendor has none. cwd is where the
// session ran and started when (Grok and Codex use them to look first
// where the file should be). id is checked with adapter.ValidSessionID
// before it becomes part of a path.
func (f *sessionFiles) LastChanged(v adapter.Vendor, id, cwd string, started time.Time) (time.Time, error) {
	if err := adapter.ValidSessionID(v, id); err != nil {
		return time.Time{}, err
	}
	root, err := f.root(v)
	if err != nil {
		return time.Time{}, err
	}
	if _, err := safefs.Stat(root); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, errNoSessionFile
		}
		return time.Time{}, err
	}
	switch v {
	case adapter.Claude:
		return f.claude(root, id)
	case adapter.Codex:
		return codexRollout(root, id, started)
	case adapter.Grok:
		return grokSession(root, id, cwd)
	}
	return time.Time{}, fmt.Errorf("unknown vendor %q", v)
}

func (f *sessionFiles) claude(root, id string) (time.Time, error) {
	if !f.listed {
		dirs, err := subdirs(root)
		if err != nil {
			return time.Time{}, err
		}
		f.claudeDirs, f.listed = dirs, true
	}
	for _, d := range f.claudeDirs {
		if t, ok := regularMtime(filepath.Join(d, id+".jsonl")); ok {
			return t, nil
		}
	}
	return time.Time{}, errNoSessionFile
}

func codexRollout(root, id string, started time.Time) (time.Time, error) {
	seen := map[string]bool{}
	for _, t := range []time.Time{started.Add(-24 * time.Hour), started, started.Add(24 * time.Hour)} {
		for _, u := range []time.Time{t.Local(), t.UTC()} {
			d := filepath.Join(root, u.Format("2006"), u.Format("01"), u.Format("02"))
			if seen[d] || !realDir(root, d) {
				continue
			}
			seen[d] = true
			es, err := safefs.ReadDir(d)
			if err != nil {
				continue
			}
			for _, e := range es {
				n := e.Name()
				if strings.HasPrefix(n, "rollout-") && strings.HasSuffix(n, "-"+id+".jsonl") {
					if t, ok := regularMtime(filepath.Join(d, n)); ok {
						return t, nil
					}
				}
			}
		}
	}
	return time.Time{}, errNoSessionFile
}

func grokSession(root, id, cwd string) (time.Time, error) {
	dir := ""
	if cwd != "" {
		if d := filepath.Join(root, grok.EncodeCwd(cwd), id); isDirNoFollow(d) {
			dir = d
		}
	}
	if dir == "" {
		dirs, err := subdirs(root)
		if err != nil {
			return time.Time{}, err
		}
		for _, p := range dirs {
			if d := filepath.Join(p, id); isDirNoFollow(d) {
				dir = d
				break
			}
		}
	}
	if dir == "" {
		return time.Time{}, errNoSessionFile
	}
	fi, err := safefs.Lstat(dir)
	if err != nil {
		return time.Time{}, err
	}
	latest := fi.ModTime()
	es, err := safefs.ReadDir(dir)
	if err != nil {
		return latest, nil
	}
	for _, e := range es {
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		if fi, err := safefs.Lstat(filepath.Join(dir, e.Name())); err == nil && fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest, nil
}

// subdirs lists root's folders, skipping symlinks, so nothing outside
// root is searched.
func subdirs(root string) ([]string, error) {
	es, err := safefs.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		if e.IsDir() && e.Type()&fs.ModeSymlink == 0 {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	return out, nil
}

// realDir reports whether every element of d below root is a folder and
// not a symlink.
func realDir(root, d string) bool {
	rel, err := filepath.Rel(root, d)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	p := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		p = filepath.Join(p, part)
		if !isDirNoFollow(p) {
			return false
		}
	}
	return true
}

func isDirNoFollow(p string) bool {
	fi, err := safefs.Lstat(p)
	return err == nil && fi.IsDir()
}

// regularMtime is the modified time of p when it is a regular file, not
// a symlink.
func regularMtime(p string) (time.Time, bool) {
	fi, err := safefs.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return time.Time{}, false
	}
	return fi.ModTime(), true
}

// refreshChildren brings the parent's open interactive children up to
// date from the disk, so their state survives a crash or sleep of aggy:
//
//   - a child whose directory was removed (its worktree, say) is closed,
//     since Claude ties a session to its directory and aggy launches a
//     child in its own;
//   - a child's last-active time (and its parent's) rises to its session
//     file's modified time;
//   - a child the vendor has no session file for is gone, unless a live
//     process holds its lease (a new session not yet saved).
//
// Lookup errors leave a child as it was. It returns the children as the
// index now holds them.
func refreshChildren(ctx context.Context, s *store.Store, parentID string, files *sessionFiles) ([]store.Child, error) {
	cs, err := s.Children(ctx, parentID)
	if err != nil {
		return nil, err
	}
	changed := false
	for _, c := range cs {
		if c.Kind != "" || c.Status != "open" {
			continue
		}
		if fi, err := safefs.Stat(c.Cwd); errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.IsDir()) {
			if err := s.SetChildActivity(ctx, c.ID, time.Time{}, "closed"); err != nil {
				return nil, err
			}
			changed = true
			continue
		}
		v, err := adapter.ParseVendor(c.Vendor)
		if err != nil || c.VendorSessionID == "" {
			continue
		}
		t, err := files.LastChanged(v, c.VendorSessionID, c.Cwd, c.CreatedAt)
		switch {
		case err == nil && t.After(c.LastActive):
			err = s.SetChildActivity(ctx, c.ID, t, "")
		case errors.Is(err, errNoSessionFile):
			if _, alive, lerr := s.LeaseHolder(ctx, c.Vendor, c.VendorSessionID); lerr != nil || alive {
				continue
			}
			err = s.SetChildActivity(ctx, c.ID, time.Time{}, "gone")
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		changed = true
	}
	if !changed {
		return cs, nil
	}
	return s.Children(ctx, parentID)
}
