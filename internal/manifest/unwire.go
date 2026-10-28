package manifest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/diff"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/termsafe"
)

// Selection picks the changes aggy unwire undoes. Exactly one field is
// set.
type Selection struct {
	ChangeID string // one change
	RunID    string // every change of one wire run
	All      bool   // every change
}

func (s Selection) validate() error {
	n := 0
	if s.ChangeID != "" {
		n++
	}
	if s.RunID != "" {
		n++
	}
	if s.All {
		n++
	}
	if n != 1 {
		return errors.New("manifest: pick exactly one of a change, a run, or all")
	}
	return nil
}

// UnwireOptions changes how Unwire acts.
type UnwireOptions struct {
	// DryRun writes the diffs an undo would make and changes nothing.
	DryRun bool
	// Force undoes a change even when its file changed after aggy changed
	// it. The file's current bytes are first copied into the change's
	// backup folder. It is for one change the person confirmed; the CLI
	// allows it only with --change and --yes.
	Force bool
}

// Outcome is what Unwire did, or in a dry run would do, with one change.
type Outcome string

// Outcomes.
const (
	// Restored: the file's old bytes and mode are back.
	Restored Outcome = "restored"
	// Removed: the file aggy created is gone, with any directories made
	// for it that are now empty.
	Removed Outcome = "removed"
	// Cleaned: a pending or failed change whose write never happened.
	// The file is as it was; any directories made for it that are empty
	// are removed.
	Cleaned Outcome = "cleaned"
	// Drifted: the file changed after aggy changed it, so the change was
	// not undone. Err holds a *patch.DriftError.
	Drifted Outcome = "drifted"
	// Superseded: a pending or failed change to a file that a newer
	// change, still in place, has written since, where some newer change
	// was planned on this change's hash before, which proves this one had
	// not written by then. The file belongs to the newer change and is
	// left alone; only empty directories made for this change are
	// removed, and the row is marked undone.
	Superseded Outcome = "superseded"
	// Blocked: a newer change to the same file is still in place, so
	// this one cannot be undone before it. Err names the newer change.
	Blocked Outcome = "blocked"
	// Invalid: the manifest row does not look like a change aggy could
	// have made (a relative or unclean file, a backup not named
	// <change-id>/<hash>.bak, a created directory that is not an
	// ancestor of the file, or that is the home directory or above it),
	// so it is refused untouched. Err says why.
	Invalid Outcome = "invalid"
	// Failed: the undo hit another error, in Err.
	Failed Outcome = "failed"
)

// ErrNewerChange matches a Blocked result.
var ErrNewerChange = errors.New("a newer change to the same file is still in place")

// ErrInvalidRow matches an Invalid result.
var ErrInvalidRow = errors.New("invalid manifest row")

// Undo is the result for one change.
type Undo struct {
	Change  store.Change
	Outcome Outcome
	// Saved is where a forced undo copied the file's drifted bytes.
	Saved string
	Err   error
}

// Unwire undoes the selected changes, newest first (reverse id order, as
// store.ChangesNewestFirst returns them): a later change to a file was
// planned against the earlier one's result, so it must be undone first.
// Each change is undone under its target's lock and marked undone.
//
// Before acting on a row, Unwire checks that it looks like a change aggy
// could have made: File absolute and clean, the backup named
// <change-id>/<hash before>.bak, and every created directory an ancestor
// of File and below the home directory. Any other row is refused
// untouched (Invalid). This is a sanity check against a damaged or
// mistaken row, not a defense: whoever can write the state directory can
// plant a well-formed row and its backup together. The backup is always
// read from the current backup root, so a state directory spelled
// differently than when the change was made still works.
//
// A change can only be undone after every newer change to the same file
// (by canonical path) that is still applied, pending, or failed:
//
//   - With --change (sel.ChangeID), such a newer change refuses the undo,
//     forced or not (Blocked, naming it). Undo it first, or use --all.
//   - An applied change with such a newer change outside the selection
//     is Blocked too.
//   - A pending or failed change that such a newer change was planned on
//     (the newer change's hash before is this change's hash after) may
//     have written before its process stopped, and that write is under
//     the newer change: it is Blocked too. --all undoes the newer one
//     first and so handles it.
//   - A pending or failed change that some newer change to the same file
//     (in any status) was planned on the same hash before as it is
//     Superseded: that newer change read the file under the target lock
//     and found this change's write had not happened, so whatever is in
//     the file now is the newer changes', the file is left alone, only
//     empty directories made for the old change are removed, and the row
//     is marked undone.
//   - Any other pending or failed change with such a newer change is
//     Blocked: nothing proves its write did not happen (it may have
//     written, been edited by hand, and had the newer change planned on
//     the edit), so it waits for the newer change. Once that is undone,
//     the file is compared with this change's hashes as below, and an
//     edited file is reported as Drifted rather than silently kept.
//
// Otherwise:
//
//   - applied: the file is restored byte for byte from the backup, with
//     its mode, or, when the change created it, removed along with the
//     directories made for it while they are empty.
//   - pending or failed: the row was written before the file, so the
//     write may or may not have happened (a pending row that outlives
//     its process means the process stopped mid-change; the target lock
//     means a live Apply is waited for, not raced). Unwire compares the
//     file with the row's hashes. Still the hash before: nothing was
//     written, and only directories made for a new file are removed while
//     empty (Cleaned). The hash after: the write happened, and it is
//     undone like an applied change, from backups/<change-id>. Anything
//     else is drift. Either way, nothing the change made is left behind.
//   - noop, undone, and marker rows are skipped and not reported.
//
// A file that changed after aggy changed it is not touched: its result
// is Drifted, and Unwire goes on with the other changes. With
// opt.Force it is undone anyway, after its bytes are saved. Two edits
// count as the person undoing aggy's change by hand, not as drift: a
// file aggy created that now holds only white space is removed, and a
// file that is its old content plus one line ending (the one aggy added
// before its own line) gets its old content back exactly.
//
// After a run that changed anything, Unwire sweeps the directories every
// undone change created and removes those that are now empty, unless a
// change still in place created them too, so undoing runs one at a time
// leaves no directory behind. It sweeps only directories that still have
// their recorded identity and that this call is related to; see
// sweepDirs.
//
// In a dry run, Unwire writes to w the unified diff each undo would make,
// escaped for the terminal, and changes nothing. The returned error joins
// every result's error.
func (m *Manifest) Unwire(ctx context.Context, sel Selection, opt UnwireOptions, w io.Writer) ([]Undo, error) {
	if err := sel.validate(); err != nil {
		return nil, err
	}
	var rows []store.Change
	switch {
	case sel.ChangeID != "":
		c, err := m.store.GetChange(ctx, sel.ChangeID)
		if err != nil {
			return nil, fmt.Errorf("manifest: change %s: %w", sel.ChangeID, err)
		}
		rows = []store.Change{c}
	default:
		var err error
		if rows, err = m.store.ChangesNewestFirst(ctx, sel.RunID); err != nil {
			return nil, err
		}
	}
	var out []Undo
	var errs []error
	// A dry run changes nothing, so an older change to a file must be
	// previewed against what the newer undos would leave, not against the
	// file on disk.
	run := &unwireRun{opt: opt, single: sel.ChangeID != "", w: w, undoneNow: map[string]bool{}}
	if opt.DryRun {
		run.sim = map[string]simFile{}
		run.simUndone = map[string]bool{}
	}
	for _, c := range rows {
		if !undoable(c) {
			continue
		}
		u := m.undo(ctx, c, run)
		if u.Change.ChangeID == "" {
			continue // no longer undoable once locked
		}
		out = append(out, u)
		if u.Err != nil {
			errs = append(errs, u.Err)
		}
	}
	if !opt.DryRun && len(out) > 0 {
		if err := m.sweepDirs(context.WithoutCancel(ctx), run); err != nil {
			errs = append(errs, err)
		}
	}
	return out, errors.Join(errs...)
}

// unwireRun is the state of one Unwire call.
type unwireRun struct {
	opt    UnwireOptions
	single bool // one change was asked for by id
	w      io.Writer
	// In a dry run: files as the earlier previewed undos would leave
	// them, and the changes they would mark undone.
	sim       map[string]simFile
	simUndone map[string]bool
	// Outside a dry run: the changes this call marked undone, and the
	// files it removed (that were there to remove). The sweep removes
	// only directories this call is related to through them.
	undoneNow map[string]bool
	emptied   []string
}

// validRow reports why c is not a change aggy could have made, or nil.
func (m *Manifest) validRow(c store.Change) error {
	bad := func(format string, a ...any) error {
		return fmt.Errorf("manifest: change %s: %w: %s", c.ChangeID, ErrInvalidRow, fmt.Sprintf(format, a...))
	}
	if c.ChangeID == "" || c.ChangeID == "." || c.ChangeID == ".." || filepath.Base(c.ChangeID) != c.ChangeID {
		return bad("the change id is not a plain name")
	}
	if !filepath.IsAbs(c.File) || filepath.Clean(c.File) != c.File {
		return bad("the file %q is not an absolute, clean path", c.File)
	}
	if !isHash(c.HashAfter) || (c.HashBefore != patch.NoFile && !isHash(c.HashBefore)) {
		return bad("the hashes are not sha256 hashes")
	}
	if (c.HashBefore == patch.NoFile) != c.CreatedFile {
		return bad("created_file does not match the hash before")
	}
	// Only the backup's shape is checked: <change-id>/<hash>.bak. Its
	// prefix is the state directory as it was spelled then, which can
	// differ now (/tmp and /private/tmp, or a state directory moved and
	// linked back), so the backup is read from the current backup root.
	if c.BackupPath != "" {
		if c.HashBefore == patch.NoFile || filepath.Base(c.BackupPath) != c.HashBefore+".bak" ||
			filepath.Base(filepath.Dir(c.BackupPath)) != c.ChangeID {
			return bad("the backup %q is not <change-id>/<hash before>.bak", c.BackupPath)
		}
	}
	homes := homeDirs()
	for _, d := range c.CreatedDirs {
		if !filepath.IsAbs(d) || filepath.Clean(d) != d || d == string(filepath.Separator) ||
			!strings.HasPrefix(c.File, d+string(filepath.Separator)) {
			return bad("the created directory %q is not an ancestor of the file", d)
		}
		for _, h := range homes {
			if h == d || strings.HasPrefix(h, d+string(filepath.Separator)) {
				return bad("the created directory %q is the home directory or above it", d)
			}
		}
	}
	if len(c.CreatedDirs) > 0 && !c.CreatedFile {
		return bad("directories are recorded for a file that existed")
	}
	return nil
}

// homeDirs returns the home directory as $HOME and as the system reports
// it, cleaned, for validRow.
func homeDirs() []string {
	var hs []string
	if h := os.Getenv("HOME"); filepath.IsAbs(h) {
		hs = append(hs, filepath.Clean(h))
	}
	if h, err := os.UserHomeDir(); err == nil && filepath.IsAbs(h) {
		hs = append(hs, filepath.Clean(h))
	}
	return hs
}

func isHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	for _, r := range h {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// newerChange returns the oldest change to the same file as c, newer
// than c, that is still applied, pending, or failed (and, in a dry run,
// not undone by an earlier previewed undo), or nil. builtOn reports
// whether any such change was planned on c's result (its hash before is
// c's hash after): then c's write is underneath it.
//
// notWritten reports whether any newer change to the same file, in any
// status, was planned on c's hash before. Apply plans under the target
// lock, which c's own Apply held until its process stopped, so that
// newer change saw the file as c found it: c's write had not happened
// (or had already been taken out by hand). Only then may c be
// superseded.
func (m *Manifest) newerChange(ctx context.Context, c store.Change, run *unwireRun) (first *store.Change, builtOn, notWritten bool, err error) {
	all, err := m.store.Changes(ctx, "")
	if err != nil {
		return nil, false, false, err
	}
	key := simKey(c.File)
	for _, n := range all {
		if n.ID > c.ID && n.Kind != store.KindMarker && n.HashBefore == c.HashBefore &&
			(n.File == c.File || simKey(n.File) == key) {
			notWritten = true
			break
		}
	}
	first, builtOn, err = m.newerInPlace(ctx, c, run)
	return first, builtOn, notWritten, err
}

// newerInPlace is newerChange without the notWritten scan.
func (m *Manifest) newerInPlace(ctx context.Context, c store.Change, run *unwireRun) (first *store.Change, builtOn bool, err error) {
	newer, err := m.store.UndoableAfter(ctx, c.ID)
	if err != nil {
		return nil, false, err
	}
	key := simKey(c.File)
	for i := range newer {
		n := newer[i]
		if run.simUndone[n.ChangeID] {
			continue
		}
		if n.File == c.File || simKey(n.File) == key {
			if first == nil {
				first = &n
			}
			if n.HashBefore == c.HashAfter && c.HashAfter != c.HashBefore {
				builtOn = true
			}
		}
	}
	return first, builtOn, nil
}

// sweepDirs removes the directories undone changes created that are now
// empty, unless a change still in place created them too.
//
// A directory is aggy's only while it has the identity recorded when
// aggy made it: device and inode, and the birth time where the file
// system reports one (macOS and the BSDs; not Linux, where the standard
// library has no statx). Device and inode alone do not prove it: ext4
// hands a freed inode number to the next directory made, so a directory
// the person removed and made again can match. So a directory is swept
// only when both hold:
//
//   - its identity matches, birth time included when one was recorded;
//   - this Unwire call is related to it: it undid the change that created
//     the directory, or it removed a file aggy wrote inside it (the undo
//     that left it empty). An unrelated later run never sweeps it.
//
// A directory the person made again in between that they left empty, or
// with nothing of aggy's in it, is therefore kept: on Linux only an undo
// of a change inside it could take it, and that undo found aggy's file
// there. Once a recorded directory is gone, or has another identity, its
// identity is cleared in the row, so a later directory that happens to
// get the same inode number is never taken for it.
func (m *Manifest) sweepDirs(ctx context.Context, run *unwireRun) error {
	all, err := m.store.Changes(ctx, "")
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, c := range all {
		if undoable(c) {
			for _, d := range c.CreatedDirs {
				live[d] = true
			}
		}
	}
	related := func(c store.Change, d string) bool {
		if run.undoneNow[c.ChangeID] {
			return true
		}
		for _, f := range run.emptied {
			if strings.HasPrefix(f, d+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	var errs []error
	for i := len(all) - 1; i >= 0; i-- {
		c := all[i]
		if c.Status != store.ChangeUndone || len(c.CreatedDirIDs) == 0 || m.validRow(c) != nil {
			continue
		}
		ids := append([]store.DirID(nil), c.CreatedDirIDs...)
		ours := func(k int) bool {
			if k >= len(ids) || ids[k] == (store.DirID{}) {
				return false
			}
			cur, ok := dirIdentity(c.CreatedDirs[k])
			if !ok || cur.Dev != ids[k].Dev || cur.Ino != ids[k].Ino ||
				(ids[k].Birth != 0 && cur.Birth != ids[k].Birth) {
				ids[k] = store.DirID{}
				return false
			}
			return true
		}
		var dirs []string
		for k, d := range c.CreatedDirs {
			if !live[d] && related(c, d) && ours(k) {
				dirs = append(dirs, d)
			}
		}
		if err := patch.RemoveEmptyDirs(dirs); err != nil {
			errs = append(errs, err)
		}
		for k := range c.CreatedDirs {
			ours(k) // clears the identity of each directory now gone
		}
		if !slices.Equal(ids, c.CreatedDirIDs) {
			if err := m.store.SetCreatedDirIDs(ctx, c.ChangeID, c.CreatedDirs, ids); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// userReverted reports whether cur is aggy's change undone by hand: a
// created file left holding only white space, or the old content plus
// the one line ending aggy added before its own line.
func userReverted(r patch.Record, cur []byte, curHash string) bool {
	if curHash == patch.NoFile {
		return false
	}
	if r.BeforeHash == patch.NoFile {
		return len(bytes.TrimSpace(cur)) == 0
	}
	for _, nl := range []string{"\n", "\r\n"} {
		if b, ok := bytes.CutSuffix(cur, []byte(nl)); ok && patch.Hash(b) == r.BeforeHash {
			return true
		}
	}
	return false
}

// undoable reports whether c is a row Unwire acts on.
func undoable(c store.Change) bool {
	if c.Kind == store.KindMarker {
		return false
	}
	switch c.Status {
	case store.ChangeApplied, store.ChangePending, store.ChangeFailed:
		return true
	}
	return false
}

// undoRecord is the patch.Record that undoes c. The backup is read from
// where Apply puts it under the current backup root,
// backups/<change-id>/<hash before>.bak, whatever prefix the row
// recorded: a pending or failed row has none yet, and the state
// directory may be spelled differently now.
func (m *Manifest) undoRecord(c store.Change) patch.Record {
	r := Record(c)
	r.Changed = true
	r.BackupPath = ""
	if r.BeforeHash != patch.NoFile {
		r.BackupPath = filepath.Join(m.backupRoot, c.ChangeID, r.BeforeHash+".bak")
	}
	return r
}

// simFile is a file's content as a dry run's earlier undos would leave it.
type simFile struct {
	data []byte
	hash string
}

// simKey names a file in a dry run's simulation, the same for every
// spelling of it where that can be found.
func simKey(file string) string {
	if k, err := safefs.CanonicalPath(file); err == nil {
		return k
	}
	return file
}

// undo undoes one change under its target's lock. It returns a zero Undo
// when the row, read again under the lock, is no longer undoable.
func (m *Manifest) undo(ctx context.Context, c store.Change, run *unwireRun) Undo {
	opt, w := run.opt, run.w
	fail := func(err error) Undo { return Undo{Change: c, Outcome: Failed, Err: err} }
	if err := m.validRow(c); err != nil {
		return Undo{Change: c, Outcome: Invalid, Err: err}
	}
	unlock, err := m.lockTarget(ctx, c.File)
	if err != nil {
		return fail(err)
	}
	defer unlock()
	if c, err = m.store.GetChange(ctx, c.ChangeID); err != nil {
		return fail(err)
	}
	if !undoable(c) {
		return Undo{}
	}
	if err := m.validRow(c); err != nil {
		return Undo{Change: c, Outcome: Invalid, Err: err}
	}
	r := m.undoRecord(c)
	u := Undo{Change: c}
	done := func() Undo {
		if opt.DryRun {
			run.simUndone[c.ChangeID] = true
			return u
		}
		// Record it even if the caller has given up.
		if merr := m.store.MarkChangeUndone(context.WithoutCancel(ctx), c.ChangeID, m.now()); merr != nil {
			u.Err = errors.Join(merr, u.Err)
		} else if run.undoneNow != nil {
			run.undoneNow[c.ChangeID] = true
		}
		return u
	}

	newer, builtOn, notWritten, err := m.newerChange(ctx, c, run)
	if err != nil {
		return fail(err)
	}
	if newer != nil {
		// A pending or failed change may have written before the process
		// stopped. Unless a newer change was planned on its hash before,
		// which proves it had not, that write may be underneath (planned
		// on its result, or on a hand edit of it), and only undoing the
		// newer one first (as --all does, newest first) can take it out
		// or show it as drift.
		if run.single || c.Status == store.ChangeApplied || builtOn || !notWritten {
			u.Outcome = Blocked
			u.Err = fmt.Errorf("manifest: change %s to %s: %w: change %s; undo it first, or use --all",
				c.ChangeID, c.File, ErrNewerChange, newer.ChangeID)
			if opt.DryRun {
				fmt.Fprintf(w, "# %s: change %s waits for the newer change %s; would not undo\n",
					termsafe.Line(r.File), termsafe.Line(c.ChangeID), termsafe.Line(newer.ChangeID))
			}
			return u
		}
		// A pending or failed change a newer change found unwritten:
		// whatever is in the file is the newer changes'.
		u.Outcome = Superseded
		if opt.DryRun {
			fmt.Fprintf(w, "# %s: change %s is superseded by change %s; would leave the file\n",
				termsafe.Line(r.File), termsafe.Line(c.ChangeID), termsafe.Line(newer.ChangeID))
			printDirs(w, r.CreatedDirs)
			return done()
		}
		if err := patch.RemoveEmptyDirs(r.CreatedDirs); err != nil {
			return fail(err)
		}
		return done()
	}

	cur, curHash, err := readFile(r.File)
	if err != nil {
		return fail(err)
	}
	if f, ok := run.sim[simKey(r.File)]; ok {
		cur, curHash = f.data, f.hash
	}
	byHand := false
	switch {
	case curHash == r.BeforeHash:
		u.Outcome = Cleaned
		if c.Status == store.ChangeApplied {
			// Restored by hand, or a half-finished undo.
			u.Outcome = Restored
			if r.BeforeHash == patch.NoFile {
				u.Outcome = Removed
			}
		}
	case curHash == r.AfterHash || opt.Force || userReverted(r, cur, curHash):
		byHand = curHash != r.AfterHash && !opt.Force
		u.Outcome = Restored
		if r.BeforeHash == patch.NoFile {
			u.Outcome = Removed
		}
	default:
		u.Outcome = Drifted
		u.Err = fmt.Errorf("manifest: change %s: %w", c.ChangeID,
			&patch.DriftError{File: r.File, Expected: r.AfterHash, Actual: curHash})
		if opt.DryRun {
			fmt.Fprintf(w, "# %s: changed since aggy changed it (change %s); would not undo\n",
				termsafe.Line(r.File), termsafe.Line(c.ChangeID))
		}
		return u
	}
	if opt.DryRun {
		target, exists, err := previewUndo(w, r, cur, curHash)
		if err != nil {
			return fail(err)
		}
		f := simFile{data: target, hash: patch.NoFile}
		if exists {
			f.hash = patch.Hash(target)
		}
		run.sim[simKey(r.File)] = f
		return done()
	}
	if (opt.Force || byHand) && curHash != r.AfterHash && curHash != r.BeforeHash {
		u.Saved, err = patch.RevertForce(r, filepath.Join(m.backupRoot, c.ChangeID))
		if byHand {
			u.Saved = "" // nothing of the person's was overwritten
		}
	} else {
		err = patch.Revert(r)
	}
	if err != nil && !errors.Is(err, safefs.ErrDirSync) {
		u.Outcome, u.Err = Failed, err
		return u
	}
	u.Err = err
	if u.Outcome == Removed && curHash != patch.NoFile {
		run.emptied = append(run.emptied, r.File)
	}
	return done()
}

// printDirs writes the directories a dry run would remove while empty.
func printDirs(w io.Writer, dirs []string) {
	for i := len(dirs) - 1; i >= 0; i-- {
		fmt.Fprintf(w, "# %s: would remove the directory if empty\n", termsafe.Line(dirs[i]))
	}
}

// readFile returns the file's bytes and hash, or nil and patch.NoFile.
func readFile(path string) ([]byte, string, error) {
	b, err := safefs.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, patch.NoFile, nil
	}
	if err != nil {
		return nil, "", err
	}
	return b, patch.Hash(b), nil
}

// previewUndo writes the diff from the file's current content to what the
// undo leaves, and the directories it would remove, and returns what the
// undo leaves.
func previewUndo(w io.Writer, r patch.Record, cur []byte, curHash string) ([]byte, bool, error) {
	// Already as before (a pending or failed change that never wrote, or
	// an undo done by hand): the content stays, and a change that failed
	// before its backup has none to read.
	target, exists := cur, curHash != patch.NoFile
	if curHash != r.BeforeHash {
		var err error
		if target, exists, err = patch.Target(r); err != nil {
			return nil, false, err
		}
	}
	from, to := r.File, r.File
	if curHash == patch.NoFile {
		from = diff.DevNull
	}
	if !exists {
		to = diff.DevNull
	}
	fmt.Fprint(w, termsafe.Lines(diff.Unified(from, to, cur, target)))
	if exists && r.Mode != 0 {
		if fi, err := safefs.Stat(r.File); err == nil && fi.Mode().Perm() != r.Mode {
			fmt.Fprintf(w, "# %s: mode %#o -> %#o\n", termsafe.Line(r.File), fi.Mode().Perm(), r.Mode)
		}
	}
	if !exists {
		printDirs(w, r.CreatedDirs)
	}
	return target, exists, nil
}
