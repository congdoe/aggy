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
	"sync/atomic"
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
	// maxCodexScan bounds the rollouts, newest by name, that one scan stats.
	maxCodexScan = 400
	// maxNativeChecked bounds the sessions of one vendor whose head one list
	// or -c reads to check them (checkNative).
	maxNativeChecked = 400
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

// nativeSessions lists v's sessions for the folder cwd, newest first (one
// modified past the future limit last, nativeFuture), at most maxNativeListed,
// each checked to be the folder's (checkNative). A vendor home or session
// folder that is not there lists none.
func nativeSessions(files *sessionFiles, v adapter.Vendor, cwd string) ([]nativeSession, error) {
	cands := scanNative(files, v, cwd)
	sortNative(cands, time.Now())
	var out []nativeSession
	for i, n := range cands {
		if len(out) == maxNativeListed || i == maxNativeChecked {
			break
		}
		if checkNative(&n, cwd, true) {
			out = append(out, n)
		}
	}
	return out, nil
}

// nativeFuture reports whether n was modified past now and store.FutureSlack: a
// clock that was wrong, or a file's time set by hand (PR #127 review). Such a
// session is listed last and never wins -c.
func nativeFuture(n nativeSession, now time.Time) bool {
	return n.Modified.After(now.Add(store.FutureSlack))
}

// sortNative sorts ns newest first, those past the future limit last.
func sortNative(ns []nativeSession, now time.Time) {
	slices.SortStableFunc(ns, func(a, b nativeSession) int {
		if fa, fb := nativeFuture(a, now), nativeFuture(b, now); fa != fb {
			if fa {
				return 1
			}
			return -1
		}
		if n := b.Modified.Compare(a.Modified); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
}

// nativeHeadReads counts the session files whose head was read (checkNative);
// a test reads it to see what a list or -c costs.
var nativeHeadReads atomic.Int64

// checkNative reports whether n, a session scanNative found for cwd, is that
// folder's, from the head of its file, and with title set, sets n.Title from
// the same read (PR #127 review):
//
//   - Claude: the folder name is not one-to-one (my_app and my-app share one,
//     and CLAUDE_CODE_PROJECT_DIR_NAME can name one for every folder), so the
//     file must record this folder as its cwd (transcript.ClaudeCwd); a file
//     that records none is no folder's.
//   - Codex: the rollout's session_meta must name the interactive TUI as its
//     source and this folder as its cwd (rolloutInDir).
//   - Grok: its folder name is the cwd url-encoded, one-to-one, and the session
//     files record no cwd in a field aggy has a source for, so the folder
//     scanNative found it in is its folder; nothing is read but the title.
func checkNative(n *nativeSession, cwd string, title bool) bool {
	root, path := nativeFile(*n, n.dir)
	switch n.Vendor {
	case adapter.Claude:
		nativeHeadReads.Add(1)
		data, err := transcript.ReadHead(root, path, nativeHead)
		if err != nil {
			return false
		}
		if c, ok := transcript.ClaudeCwd(data); !ok || !sameFolder(c, folderForms(cwd)) {
			return false
		}
		if title {
			n.Title = transcript.FirstPrompt(n.Vendor, data)
		}
		return true
	case adapter.Codex:
		nativeHeadReads.Add(1)
		if !rolloutInDir(path, n.ID, folderForms(cwd)) {
			return false
		}
	}
	if title {
		n.Title = nativeTitle(*n)
	}
	return true
}

// sameFolder reports whether recorded, a folder a session file names, is one
// of cwds (folderForms), as given or resolved.
func sameFolder(recorded string, cwds []string) bool {
	if !filepath.IsAbs(recorded) {
		return false
	}
	if slices.Contains(cwds, filepath.Clean(recorded)) {
		return true
	}
	c, err := safefs.CanonicalPath(recorded)
	return err == nil && slices.Contains(cwds, c)
}

// scanNative is every session of v found for cwd, in no order, without titles,
// and not yet checked (checkNative): what listing the session folder gives.
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
		found = codexNative(h)
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
	nativeHeadReads.Add(1)
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

func codexNative(home string) []nativeSession {
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
	var out []nativeSession
	for _, r := range rs {
		rel, err := filepath.Rel(root, r.path)
		if err != nil || !realDir(root, filepath.Dir(r.path)) {
			continue
		}
		// Whose folder it is, checkNative reads, for as few as are needed.
		t, ok := regularMtime(r.path)
		if !ok {
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
	// future: a native session modified past the future limit, listed last.
	future bool
}

// sessionChoices lists the sessions of vendors in the folder cwd, newest
// first: the interactive children aggy recorded in the folder's parents that
// can be reopened, and the native sessions in the vendors' own stores that no
// child of aggy's holds (one a borrow ran there is a child too, and is left
// out), a native one modified past the future limit last (nativeFuture). s may
// be nil (no index yet): then every session is native. slow says the vendors'
// own folders took longer than nativeBudget and only aggy's are listed.
func sessionChoices(ctx context.Context, s *store.Store, cwd string, vendors []adapter.Vendor, files *sessionFiles) (out []sessionChoice, slow bool, err error) {
	natives, ok := withinBudget(func() [][]nativeSession {
		nf := &sessionFiles{getenv: files.getenv, now: files.now, readOnly: true}
		var all [][]nativeSession
		for _, v := range vendors {
			ns, _ := nativeSessions(nf, v, cwd)
			all = append(all, ns)
		}
		return all
	})
	byID := map[string]int{} // vendor/session id, folded, to its choice
	if s != nil {
		key, err := safefs.CanonicalPath(cwd)
		if err != nil {
			return nil, false, err
		}
		ps, err := s.ParentsInDir(ctx, key, cwd)
		if err != nil {
			return nil, false, err
		}
		for _, par := range ps {
			if replaced(par) {
				continue
			}
			children, err := planRefresh(ctx, s, par.ID, files)
			if err != nil {
				return nil, false, err
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
				byID[c.Vendor+"/"+strings.ToLower(c.VendorSessionID)] = len(out)
				out = append(out, sessionChoice{Vendor: adapter.Vendor(c.Vendor), When: c.LastActive, Title: title, ChildID: c.ID})
			}
		}
	}
	now := time.Now()
	for _, ns := range natives {
		for i := range ns {
			n := ns[i]
			if j, ok := byID[string(n.Vendor)+"/"+strings.ToLower(n.ID)]; ok {
				if out[j].Title == "" {
					out[j].Title = n.Title
				}
				continue
			}
			if s != nil {
				if _, err := s.ChildBySession(ctx, string(n.Vendor), n.ID); err == nil {
					continue
				} else if !errors.Is(err, store.ErrNotFound) {
					return nil, false, err
				}
			}
			out = append(out, sessionChoice{Vendor: n.Vendor, When: n.Modified, Title: n.Title, Native: &n,
				future: nativeFuture(n, now)})
		}
	}
	slices.SortStableFunc(out, func(a, b sessionChoice) int {
		if a.future != b.future {
			if a.future {
				return 1
			}
			return -1
		}
		return b.When.Compare(a.When)
	})
	return out, !ok, nil
}

// nativeBudget bounds one look at the vendors' own session folders (PR #127
// review): a folder on a slow or hung disk must not hold up a launch. Past it,
// aggy goes on with its own index alone and says so (nativeSlowNote).
var nativeBudget = 3 * time.Second

// nativeScanHook, when set, runs first in each look withinBudget makes; a test
// makes the look slow with it.
var nativeScanHook func()

// nativeSlowNote is said when a look took longer than nativeBudget.
const nativeSlowNote = "the CLIs' own session folders took more than a few seconds to read, so aggy looked at its own sessions only"

// withinBudget runs fn, which must only read, and returns its result, or false
// when it took longer than nativeBudget. fn then runs on to its end unwatched,
// and its result is dropped.
func withinBudget[T any](fn func() T) (T, bool) {
	ch := make(chan T, 1)
	hook := nativeScanHook
	go func() {
		if hook != nil {
			hook()
		}
		ch <- fn()
	}()
	t := time.NewTimer(nativeBudget)
	defer t.Stop()
	select {
	case v := <-ch:
		return v, true
	case <-t.C:
		var zero T
		return zero, false
	}
}

// findNative is the native session of one of vendors in cwd with id, in any
// case, checked to be that folder's (checkNative), or false.
func findNative(files *sessionFiles, vendors []adapter.Vendor, cwd, id string) (nativeSession, bool) {
	for _, v := range vendors {
		for _, n := range scanNative(files, v, cwd) {
			if strings.EqualFold(n.ID, id) {
				if !checkNative(&n, cwd, true) {
					return nativeSession{}, false
				}
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
