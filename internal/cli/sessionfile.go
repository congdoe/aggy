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
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// sessionFiles finds a vendor session's files in the vendor's sessions
// folder and returns when they last changed. It stats and lists folders
// only: it never reads a session file, never follows a symlink inside
// the sessions folder, and goes through safefs, which refuses the
// credential files. The folders, under the vendor home recorded on the
// child at launch (else the current one):
//
//   - Claude: <$CLAUDE_CONFIG_DIR or ~/.claude>/projects/<one folder per
//     cwd>/<session id>.jsonl. Unverified: docs/verification.md does not
//     record this layout. The folder for the cwd (every character but
//     A-Z, a-z, and 0-9 made "-", also unverified) is tried first, then
//     every project folder. A miss is "unknown", never "gone".
//   - Codex: <$CODEX_HOME or ~/.codex>/sessions/<YYYY>/<MM>/<DD>/
//     rollout-<timestamp>-<thread id>.jsonl, created at the first persist
//     (codex.RolloutThreadID). Which clock names the date folder is not
//     recorded, so every date folder from the day before the child's
//     start through the day after today is searched. codex.RolloutDirs
//     covers only the days around one session, so it is not used here.
//   - Grok: <$GROK_HOME or ~/.grok>/sessions/<url-encoded cwd>/<session
//     id>/, a folder (docs/adapters.md, Grok); its newest entry counts.
//
// The zero value uses the environment.
type sessionFiles struct {
	getenv func(string) string // nil means os.Getenv
	now    func() time.Time    // nil means time.Now

	claudeDirs map[string][]string // project folders per root, listed once
}

var (
	// errNoSessionFile means the vendor's sessions folder is there, its
	// layout is recorded, and it holds no file for the session.
	errNoSessionFile = errors.New("no session file")
	// errUnknownSession means aggy cannot tell: the sessions folder is
	// missing (an unmounted disk, another vendor home) or its layout is
	// unverified.
	errUnknownSession = errors.New("session file not found where aggy looks")
)

func (f *sessionFiles) env(k string) string {
	if f.getenv != nil {
		return f.getenv(k)
	}
	return os.Getenv(k)
}

// home is the vendor's home folder in this environment: the relocation
// variable when set, else ~/.<vendor>.
func (f *sessionFiles) home(v adapter.Vendor) (string, error) {
	relocate := map[adapter.Vendor]string{adapter.Claude: "CLAUDE_CONFIG_DIR", adapter.Codex: "CODEX_HOME", adapter.Grok: "GROK_HOME"}[v]
	if relocate == "" {
		return "", fmt.Errorf("unknown vendor %q", v)
	}
	if d := f.env(relocate); d != "" {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("%s is not an absolute path", relocate)
		}
		return filepath.Clean(d), nil
	}
	h := f.env("HOME")
	if h == "" || !filepath.IsAbs(h) {
		return "", errors.New("no absolute HOME")
	}
	return filepath.Join(h, "."+string(v)), nil
}

// LastChanged returns when the session id of vendor v last changed on
// disk. vendorHome is the vendor home recorded at launch ("" means this
// environment's), cwd is where the session ran, and started when. id is
// checked with adapter.ValidSessionID before it becomes part of a path.
// The errors that say something about the session are errNoSessionFile
// and errUnknownSession.
func (f *sessionFiles) LastChanged(v adapter.Vendor, id, vendorHome, cwd string, started time.Time) (time.Time, error) {
	if err := adapter.ValidSessionID(v, id); err != nil {
		return time.Time{}, err
	}
	h := filepath.Clean(vendorHome)
	if vendorHome == "" || !filepath.IsAbs(h) {
		var err error
		if h, err = f.home(v); err != nil {
			return time.Time{}, err
		}
	}
	root := filepath.Join(h, "sessions")
	if v == adapter.Claude {
		root = filepath.Join(h, "projects")
	}
	if fi, err := safefs.Stat(root); err != nil || !fi.IsDir() {
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			return time.Time{}, errUnknownSession
		}
		return time.Time{}, err
	}
	switch v {
	case adapter.Claude:
		return f.claude(root, id, cwd)
	case adapter.Codex:
		now := time.Now()
		if f.now != nil {
			now = f.now()
		}
		return codexRollout(root, id, started, now)
	case adapter.Grok:
		return grokSession(root, id, cwd)
	}
	return time.Time{}, fmt.Errorf("unknown vendor %q", v)
}

// claudeProjectDir is the project folder Claude is thought to use for
// cwd. Unverified; only a first guess before the full scan.
func claudeProjectDir(cwd string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, cwd)
}

func (f *sessionFiles) claude(root, id, cwd string) (time.Time, error) {
	if cwd != "" {
		if d := filepath.Join(root, claudeProjectDir(cwd)); realDir(root, d) {
			if t, ok := regularMtime(filepath.Join(d, id+".jsonl")); ok {
				return t, nil
			}
		}
	}
	dirs, ok := f.claudeDirs[root]
	if !ok {
		var err error
		if dirs, err = subdirs(root); err != nil {
			return time.Time{}, err
		}
		if f.claudeDirs == nil {
			f.claudeDirs = map[string][]string{}
		}
		f.claudeDirs[root] = dirs
	}
	for _, d := range dirs {
		if t, ok := regularMtime(filepath.Join(d, id+".jsonl")); ok {
			return t, nil
		}
	}
	return time.Time{}, errUnknownSession
}

// codexRollout searches the existing date folders from the day before
// started through the day after now (which covers local time and UTC),
// listing only folders that are there.
func codexRollout(root, id string, started, now time.Time) (time.Time, error) {
	if started.IsZero() || started.After(now) {
		started = now
	}
	from := started.UTC().Add(-48 * time.Hour).Format("2006/01/02")
	to := now.UTC().Add(48 * time.Hour).Format("2006/01/02")
	var days []string
	for _, y := range dateDirs(root, 4, from[:4], to[:4]) {
		for _, m := range dateDirs(y, 2, "01", "12") {
			for _, d := range dateDirs(m, 2, "01", "31") {
				if day := strings.TrimPrefix(filepath.ToSlash(d), filepath.ToSlash(root)+"/"); day >= from && day <= to {
					days = append(days, d)
				}
			}
		}
	}
	for _, d := range days {
		es, err := safefs.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range es {
			if got, ok := codex.RolloutThreadID(e.Name()); ok && got == id {
				if t, ok := regularMtime(filepath.Join(d, e.Name())); ok {
					return t, nil
				}
			}
		}
	}
	return time.Time{}, errNoSessionFile
}

// dateDirs lists dir's folders (not symlinks) whose names are width
// digits between lo and hi.
func dateDirs(dir string, width int, lo, hi string) []string {
	es, err := safefs.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range es {
		n := e.Name()
		if len(n) != width || strings.Trim(n, "0123456789") != "" || n < lo || n > hi ||
			!e.IsDir() || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		out = append(out, filepath.Join(dir, n))
	}
	return out
}

func grokSession(root, id, cwd string) (time.Time, error) {
	dir := ""
	if cwd != "" {
		if d := filepath.Join(root, grok.EncodeCwd(cwd), id); realDir(root, d) {
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

// realDir reports whether d is below root and every element of it below
// root is a folder and not a symlink.
func realDir(root, d string) bool {
	rel, err := filepath.Rel(root, d)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
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

// Child states refreshChildren derives for open interactive children.
// They are worked out each time from the disk and the leases, never
// stored, since what they rest on can be temporary (an unmounted disk,
// another vendor home in this shell, a session not saved yet).
const (
	stateOpen    = "open"
	stateRunning = "running" // a live process holds its lease
	stateClosed  = "closed"  // its directory is gone (a removed worktree)
	stateGone    = "gone"    // the vendor's sessions folder has no file for it
	stateUnknown = "unknown" // aggy cannot tell where the file should be
)

// refreshChildren returns the parent's children with the state of each
// open interactive child in Status (see stateOpen and the rest), and
// raises last-active times, the child's and its parent's, to the session
// files' modified times, so they survive a crash or sleep of aggy. A
// child whose lease is live is running and is not checked further. Only
// last-active times are written.
func refreshChildren(ctx context.Context, s *store.Store, parentID string, files *sessionFiles) ([]store.Child, error) {
	cs, err := s.Children(ctx, parentID)
	if err != nil {
		return nil, err
	}
	raised := false
	for i := range cs {
		c := &cs[i]
		if c.Kind != "" || c.Status != "open" {
			continue
		}
		if c.VendorSessionID != "" {
			if _, alive, err := s.LeaseHolder(ctx, c.Vendor, c.VendorSessionID); err == nil && alive {
				c.Status = stateRunning
			}
		}
		if c.Status != stateRunning {
			if fi, err := safefs.Stat(c.Cwd); errors.Is(err, fs.ErrNotExist) || (err == nil && !fi.IsDir()) {
				c.Status = stateClosed
				continue
			}
		}
		v, err := adapter.ParseVendor(c.Vendor)
		if err != nil || c.VendorSessionID == "" {
			continue
		}
		t, err := files.LastChanged(v, c.VendorSessionID, c.VendorHome, c.Cwd, c.CreatedAt)
		switch {
		case err == nil:
			if t.After(c.LastActive) {
				if err := s.SetChildActivity(ctx, c.ID, t); err != nil {
					return nil, err
				}
				raised = true
			}
		case c.Status == stateRunning:
		case errors.Is(err, errNoSessionFile):
			c.Status = stateGone
		default:
			c.Status = stateUnknown
		}
	}
	if !raised {
		return cs, nil
	}
	fresh, err := s.Children(ctx, parentID)
	if err != nil {
		return nil, err
	}
	state := map[string]string{}
	for _, c := range cs {
		state[c.ID] = c.Status
	}
	for i := range fresh {
		fresh[i].Status = state[fresh[i].ID]
	}
	return fresh, nil
}
