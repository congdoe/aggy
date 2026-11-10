package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
	"github.com/congdoe/aggy/internal/transcript"
)

// nativeSession is a session found in a vendor's own session store for one
// folder (T6965): one the vendor CLI started by itself, or one aggy started
// and recorded, which the caller tells apart by its id (store.ChildBySession).
//
// How each vendor's sessions are found for a folder, by direct read of the
// vendor's documented session folder for that folder only:
//
//   - Claude: <$CLAUDE_CONFIG_DIR or ~/.claude>/projects/<project>/<session
//     id>.jsonl, where <project> is the working directory with every
//     non-alphanumeric character made "-"; a name over 200 characters is cut
//     and a hash of the full path added, and with CLAUDE_CONFIG_DIR set,
//     CLAUDE_CODE_PROJECT_DIR_NAME (1 to 64 letters, digits, "-", "_") names
//     it instead (code.claude.com/docs/en/sessions, "Where transcripts are
//     stored" and "Name the project directory yourself", read 2026-11-10).
//     The hash is not documented, and whether a non-ASCII letter counts as
//     alphanumeric is not either, so a folder whose name would need either is
//     not listed: no guess. Claude's own picker and --continue leave out
//     claude -p and Agent SDK sessions (same page); the files do not say which
//     is which in a documented field, so aggy's list may show them.
//   - Codex: <$CODEX_HOME or ~/.codex>/sessions/YYYY/MM/DD/rollout-*-<id>.jsonl
//     (rollout/src/list.rs#L423 at rust-v0.164.0), whose first line, the
//     session_meta, names the interactive TUI as its source and the folder as
//     its cwd (rolloutInDir: protocol.rs#L3275 at 6aaac694). Every date folder
//     is listed; the newest maxCodexScan rollouts by name have that line read.
//   - Grok: <$GROK_HOME or ~/.grok>/sessions/<url-encoded cwd>/<id>/
//     (grok-build 2bdd1d6a, user-guide/17-sessions.md, "Storage Layout"),
//     with chat_history.jsonl for its first prompt. The exact encoding is
//     grok.EncodeCwd's reading of "url-encoded", unconfirmed: a miss lists
//     nothing, and no other folder is scanned.
//   - OpenCode keeps its sessions in a database beside its sign-ins, which
//     aggy never opens: none are listed.
//
// Every read goes through safefs and internal/transcript, never through a
// symlink inside the session folder, and reads at most nativeHead bytes of a
// file. A file's text is the model's input or output: Title is shown only
// through sessionTitle, which escapes it.
type nativeSession struct {
	Vendor   adapter.Vendor
	ID       string
	Home     string // the vendor home it was found under
	Modified time.Time
	Title    string // the first prompt as stored; untrusted
	// dir is the session's folder under the vendor's session root (Claude's
	// project folder, Grok's encoded cwd), or for Codex the rollout's path
	// there.
	dir string
}

const (
	// maxNativeListed bounds the sessions per vendor whose first prompt is
	// read for one list.
	maxNativeListed = 50
	// maxCodexScan bounds the rollouts whose first line is read for one list.
	maxCodexScan = 400
	// nativeHead is how much of a session file is read for its first prompt.
	nativeHead = 256 << 10
)

// nativeVendors are the vendors whose session stores aggy reads.
var nativeVendors = []adapter.Vendor{adapter.Claude, adapter.Codex, adapter.Grok}

// claudeDirName is Claude's project folder name for cwd (see nativeSession),
// and false when aggy cannot tell it: a name over 200 characters, whose hash
// is not documented, or a path with a byte outside ASCII.
func claudeDirName(cwd string, getenv func(string) string) (string, bool) {
	if getenv("CLAUDE_CONFIG_DIR") != "" {
		if n := getenv("CLAUDE_CODE_PROJECT_DIR_NAME"); n != "" && projectDirNameRE.MatchString(n) {
			return n, true
		}
	}
	for i := 0; i < len(cwd); i++ {
		if cwd[i] >= 0x80 {
			return "", false
		}
	}
	name := transcript.ClaudeProjectDir(cwd)
	if name == "" || len(name) > 200 {
		return "", false
	}
	return name, true
}

// projectDirNameRE is what Claude takes for CLAUDE_CODE_PROJECT_DIR_NAME.
var projectDirNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// folderForms are cwd as given and resolved: a CLI may record either.
func folderForms(cwd string) []string {
	out := []string{filepath.Clean(cwd)}
	if c, err := safefs.CanonicalPath(cwd); err == nil && c != out[0] {
		out = append(out, c)
	}
	return out
}

// nativeSessions lists v's sessions for the folder cwd, newest first, at most
// maxNativeListed. A vendor home or session folder that is not there lists
// none.
func nativeSessions(files *sessionFiles, v adapter.Vendor, cwd string) ([]nativeSession, error) {
	out := scanNative(files, v, cwd)
	slices.SortStableFunc(out, func(a, b nativeSession) int {
		if n := b.Modified.Compare(a.Modified); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	if len(out) > maxNativeListed {
		out = out[:maxNativeListed]
	}
	for i := range out {
		out[i].Title = nativeTitle(out[i])
	}
	return out, nil
}

// scanNative is every session of v found for cwd, in no order and without
// titles.
func scanNative(files *sessionFiles, v adapter.Vendor, cwd string) []nativeSession {
	if !slices.Contains(nativeVendors, v) || cwd == "" {
		return nil
	}
	h, err := files.home(v)
	if err != nil {
		return nil
	}
	var found []nativeSession
	switch v {
	case adapter.Claude:
		found = claudeNative(files, h, cwd)
	case adapter.Codex:
		found = codexNative(h, cwd)
	case adapter.Grok:
		found = grokNative(h, cwd)
	}
	// The folder's two forms can name one folder (a case-insensitive disk):
	// each session once.
	seen := map[string]bool{}
	out := found[:0]
	for _, n := range found {
		if !seen[n.ID] {
			seen[n.ID] = true
			out = append(out, n)
		}
	}
	return out
}

// nativeFile is the file whose head holds s's first prompt, and the folder
// it must be under.
func nativeFile(s nativeSession, cwdDir string) (root, path string) {
	switch s.Vendor {
	case adapter.Claude:
		root = filepath.Join(s.Home, "projects")
		return root, filepath.Join(root, cwdDir, s.ID+".jsonl")
	case adapter.Grok:
		root = filepath.Join(s.Home, "sessions")
		return root, filepath.Join(root, cwdDir, s.ID, "chat_history.jsonl")
	}
	root = filepath.Join(s.Home, "sessions")
	return root, filepath.Join(root, cwdDir)
}

// nativeTitle reads the first prompt of s, or "" when it cannot.
func nativeTitle(s nativeSession) string {
	root, path := nativeFile(s, s.dir)
	head := int64(nativeHead)
	if s.Vendor == adapter.Codex {
		// session_meta comes first and carries the base instructions.
		head += maxMetaLine
	}
	data, err := transcript.ReadHead(root, path, head)
	if err != nil {
		return ""
	}
	return transcript.FirstPrompt(s.Vendor, data)
}

func claudeNative(files *sessionFiles, home, cwd string) []nativeSession {
	root := filepath.Join(home, "projects")
	seen := map[string]bool{}
	var out []nativeSession
	for _, c := range folderForms(cwd) {
		name, ok := claudeDirName(c, files.env)
		if !ok || seen[name] {
			continue
		}
		seen[name] = true
		dir := filepath.Join(root, name)
		if !realDir(root, dir) {
			continue
		}
		es, err := safefs.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range es {
			id, ok := strings.CutSuffix(e.Name(), ".jsonl")
			if !ok || !e.Type().IsRegular() || adapter.ValidSessionID(adapter.Claude, id) != nil {
				continue
			}
			if t, ok := regularMtime(filepath.Join(dir, e.Name())); ok {
				out = append(out, nativeSession{Vendor: adapter.Claude, ID: id, Home: home, Modified: t, dir: name})
			}
		}
	}
	return out
}

func codexNative(home, cwd string) []nativeSession {
	root := filepath.Join(home, "sessions")
	if !isDirNoFollow(root) {
		return nil
	}
	type rollout struct{ id, path string }
	var rs []rollout
	for id, r := range indexRollouts(root) {
		rs = append(rs, rollout{id, r.path})
	}
	// Newest first by name, whose timestamp sorts as text.
	slices.SortFunc(rs, func(a, b rollout) int { return strings.Compare(filepath.Base(b.path), filepath.Base(a.path)) })
	if len(rs) > maxCodexScan {
		rs = rs[:maxCodexScan]
	}
	cwds := folderForms(cwd)
	var out []nativeSession
	for _, r := range rs {
		rel, err := filepath.Rel(root, r.path)
		if err != nil || !realDir(root, filepath.Dir(r.path)) {
			continue
		}
		t, ok := regularMtime(r.path)
		if !ok || !rolloutInDir(r.path, r.id, cwds) {
			continue
		}
		out = append(out, nativeSession{Vendor: adapter.Codex, ID: r.id, Home: home, Modified: t, dir: rel})
	}
	return out
}

func grokNative(home, cwd string) []nativeSession {
	root := filepath.Join(home, "sessions")
	seen := map[string]bool{}
	var out []nativeSession
	for _, c := range folderForms(cwd) {
		name := grok.EncodeCwd(c)
		if seen[name] {
			continue
		}
		seen[name] = true
		dir := filepath.Join(root, name)
		if !realDir(root, dir) {
			continue
		}
		es, err := safefs.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range es {
			id := e.Name()
			if !e.IsDir() || e.Type()&fs.ModeSymlink != 0 || adapter.ValidSessionID(adapter.Grok, id) != nil {
				continue
			}
			if t, err := grokSession(root, id, c); err == nil {
				out = append(out, nativeSession{Vendor: adapter.Grok, ID: id, Home: home, Modified: t, dir: name})
			}
		}
	}
	return out
}

// maxTitle is how many characters of a first prompt a list shows.
const maxTitle = 60

// sessionTitle is title made safe for one line of a terminal: runs of white
// space made one space, every control or format character escaped
// (termsafe), and cut to maxTitle characters.
func sessionTitle(title string) string {
	t := termsafe.Field(strings.Join(strings.Fields(title), " "))
	if utf8.RuneCountInString(t) > maxTitle {
		r := []rune(t)
		t = string(r[:maxTitle-1]) + "…"
	}
	return t
}

// sessionChoice is one session a resume may reopen in a folder: a child aggy
// recorded, or a native session aggy has not (Native).
type sessionChoice struct {
	Vendor  adapter.Vendor
	When    time.Time
	Title   string // untrusted; shown through sessionTitle
	ChildID string // the recorded child, or ""
	Native  *nativeSession
}

// sessionChoices lists the sessions of vendors in the folder cwd, newest
// first: the interactive children aggy recorded in the folder's parents that
// can be reopened, and the native sessions in the vendors' own stores that no
// child of aggy's holds (one a borrow ran there is a child too, and is left
// out). s may be nil (no index yet): then every session is native.
func sessionChoices(ctx context.Context, s *store.Store, cwd string, vendors []adapter.Vendor, files *sessionFiles) ([]sessionChoice, error) {
	var out []sessionChoice
	byID := map[string]int{} // vendor/session id to its choice
	if s != nil {
		key, err := safefs.CanonicalPath(cwd)
		if err != nil {
			return nil, err
		}
		ps, err := s.ParentsInDir(ctx, key, cwd)
		if err != nil {
			return nil, err
		}
		for _, par := range ps {
			if replaced(par) {
				continue
			}
			children, err := planRefresh(ctx, s, par.ID, files)
			if err != nil {
				return nil, err
			}
			for _, c := range children {
				if c.Kind != "" || c.VendorSessionID == "" || c.Status == stateClosed || c.Status == stateGone ||
					!slices.Contains(vendors, adapter.Vendor(c.Vendor)) {
					continue
				}
				title := c.Title
				if title == "" {
					title = par.Title
				}
				byID[c.Vendor+"/"+c.VendorSessionID] = len(out)
				out = append(out, sessionChoice{Vendor: adapter.Vendor(c.Vendor), When: c.LastActive, Title: title, ChildID: c.ID})
			}
		}
	}
	for _, v := range vendors {
		ns, err := nativeSessions(files, v, cwd)
		if err != nil {
			return nil, err
		}
		for i := range ns {
			n := ns[i]
			if j, ok := byID[string(v)+"/"+n.ID]; ok {
				if out[j].Title == "" {
					out[j].Title = n.Title
				}
				continue
			}
			if s != nil {
				if _, err := s.ChildBySession(ctx, string(v), n.ID); err == nil {
					continue
				} else if !errors.Is(err, store.ErrNotFound) {
					return nil, err
				}
			}
			out = append(out, sessionChoice{Vendor: v, When: n.Modified, Title: n.Title, Native: &n})
		}
	}
	limit := time.Now().Add(store.FutureSlack)
	at := func(c sessionChoice) time.Time {
		if c.When.After(limit) {
			return limit
		}
		return c.When
	}
	slices.SortStableFunc(out, func(a, b sessionChoice) int { return at(b).Compare(at(a)) })
	return out, nil
}

// findNative is the native session of one of vendors in cwd with id, or false.
func findNative(files *sessionFiles, vendors []adapter.Vendor, cwd, id string) (nativeSession, bool) {
	for _, v := range vendors {
		for _, n := range scanNative(files, v, cwd) {
			if n.ID == id {
				n.Title = nativeTitle(n)
				return n, true
			}
		}
	}
	return nativeSession{}, false
}

// ago says how long before now t was, for a list.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
	return t.Local().Format("2006-01-02")
}
