// Package patch makes guarded, reversible changes to config files. A Patch
// names a file, the hash it expects to find there, and what to make of
// it: new content (a transform), a symlink, or nothing (a removal). Apply
// acts only when the path still matches, backs up what was there, and
// returns a Record that aggy unwire can use to restore it byte for byte.
//
// A content patch reads and writes through a symlink at its path, as it
// always has. A link or removal patch never follows one: it reads the
// path with lstat, records a symlink by its target (LinkHash), and acts
// only on a path that still has the identity it checked.
//
// All file access goes through safefs, so vendor credential files are
// refused here too.
package patch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/safefs"
)

// NoFile is the hash of a file that does not exist. Set it as
// Patch.ExpectedHash when the file must not exist yet.
const NoFile = "none"

// DefaultMode is the mode of a file a patch creates when Patch.Mode is zero.
const DefaultMode fs.FileMode = 0o600

// Patch is one guarded change to one file.
type Patch struct {
	// File is the path to change.
	File string
	// ExpectedHash is the hex sha256 the file must have before the change,
	// or NoFile when it must not exist. It is required.
	ExpectedHash string
	// Transform returns the new content. It gets nil when the file does not
	// exist. It must be deterministic: applying it to its own output must
	// return that output unchanged, which is how a repeat apply is detected.
	Transform func([]byte) ([]byte, error)
	// Mode is the mode of a newly created file. An existing file keeps its
	// mode. Zero means DefaultMode.
	Mode fs.FileMode
	// Link, when set, makes File a symlink to Link instead: it creates the
	// link, or replaces the file or link there (ExpectedHash names it;
	// see LinkHash). Transform must be nil. A Link that is, or resolves
	// to, a vendor credential file is refused, and so is replacing one.
	Link string
	// Remove, when true, removes File: a regular file, or a symlink
	// itself, never what it points to. Transform must be nil and Link
	// empty. Undo puts back the same file or the same link.
	Remove bool
}

// kinds counts the change kinds p asks for; exactly one is valid.
func (p Patch) kinds() int {
	n := 0
	for _, set := range []bool{p.Transform != nil, p.Link != "", p.Remove} {
		if set {
			n++
		}
	}
	return n
}

// Record describes an applied change in the shape of the store's changes
// table: file, hash before, hash after, backup path, and what an exact undo
// needs besides the old bytes.
type Record struct {
	File       string // absolute path
	BeforeHash string // hex sha256, or NoFile when the patch created the file
	AfterHash  string // hex sha256 after the change
	BackupPath string // copy of the old bytes; empty when BeforeHash is NoFile or nothing changed
	Changed    bool   // false when the file already held the patched content
	// Created is true when the patch created the file.
	Created bool
	// Mode is the file's permission bits before the change, or those it was
	// created with.
	Mode fs.FileMode
	// CreatedDirs lists the directories Apply created for a new file,
	// outermost first. Revert removes them again while they are empty.
	CreatedDirs []string
}

// linkPrefix starts every LinkHash. A file's hash is bare hex, so a
// symlink's hash never equals a file's.
const linkPrefix = "link-"

// LinkHash is the hash recorded for a symlink to target: "link-" and the
// hex sha256 of the target string. A link is identified by its target
// alone; what it points to is never read.
func LinkHash(target string) string { return linkPrefix + Hash([]byte(target)) }

// IsLinkHash reports whether h is a LinkHash.
func IsLinkHash(h string) bool { return strings.HasPrefix(h, linkPrefix) }

// NoFollow reports whether r's path is read without following a final
// symlink: a change that makes or replaces a symlink, replaces one, or
// removes a file or link. A content change can never leave NoFile, so
// AfterHash NoFile marks a removal.
func (r Record) NoFollow() bool {
	return IsLinkHash(r.BeforeHash) || IsLinkHash(r.AfterHash) || r.AfterHash == NoFile
}

// State is what is at a path, read without following a final symlink.
type State struct {
	Data []byte      // a regular file's bytes, or a symlink's target
	Hash string      // Hash of a file, LinkHash of a link, or NoFile
	Info fs.FileInfo // the Lstat of what is there; nil for NoFile
}

// testHookAfterLstat runs in ReadState between the Lstat and the read of
// a regular file; tests use it to swap the path.
var testHookAfterLstat func(path string)

// ReadState reads path without following a final symlink. Anything but a
// regular file, a symlink, or nothing is an error, and so is a file
// swapped for another between the Lstat and the read.
func ReadState(path string) (State, error) {
	fi, err := safefs.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{Hash: NoFile}, nil
	}
	if err != nil {
		return State{}, err
	}
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		t, err := safefs.Readlink(path)
		if err != nil {
			return State{}, err
		}
		return State{Data: []byte(t), Hash: LinkHash(t), Info: fi}, nil
	case fi.Mode().IsRegular():
		if testHookAfterLstat != nil {
			testHookAfterLstat(path)
		}
		// Never through a symlink, and only the file this Lstat saw.
		b, err := safefs.ReadFileNoFollow(path, fi)
		if err != nil {
			return State{}, err
		}
		return State{Data: b, Hash: Hash(b), Info: fi}, nil
	}
	return State{}, &fs.PathError{Op: "read", Path: path, Err: fmt.Errorf("not a file or symlink: %v", fi.Mode().Type())}
}

// Current returns what r.File holds now, read the way r's kind reads it:
// without following a symlink for NoFollow records, through it otherwise.
// It returns nil and NoFile when nothing is there.
func Current(r Record) ([]byte, string, error) {
	if r.NoFollow() {
		st, err := ReadState(r.File)
		return st.Data, st.Hash, err
	}
	return read(r.File)
}

// Describe names a state for a preview: a symlink and its target, a
// file, or nothing.
func Describe(hash string, data []byte) string {
	switch {
	case hash == NoFile:
		return "nothing"
	case IsLinkHash(hash):
		return "symlink to " + string(data)
	}
	return fmt.Sprintf("file (%d bytes, %s)", len(data), short(hash))
}

// Action is what applying a patch would do.
type Action string

// Actions reported by Plan.
const (
	Create Action = "create" // the file does not exist and would be created
	Modify Action = "modify" // the file would be rewritten, or the file or link replaced by a link
	Delete Action = "remove" // the file or link would be removed
	NoOp   Action = "noop"   // the file already holds the patched content
)

// Planned is the dry-run result of a patch.
type Planned struct {
	Record
	Action Action
	Before []byte // current content (a link's target), nil when nothing is there
	After  []byte // content after the change (a link's target)
	// info is the Lstat of what a link or removal patch found.
	info fs.FileInfo
}

// ErrNotIdempotent means applying the transform to its own output changed
// it again. Such a patch is refused and nothing is written.
var ErrNotIdempotent = errors.New("transform is not idempotent")

// ErrDrift matches, through errors.Is, every *DriftError.
var ErrDrift = errors.New("file changed since the patch was planned")

// DriftError reports a file whose hash is neither the expected one nor
// already patched, or, on revert, no longer the hash the patch left.
type DriftError struct {
	File     string
	Expected string
	Actual   string
}

func (e *DriftError) Error() string {
	return fmt.Sprintf("patch: %s: %v (expected %s, found %s)", e.File, ErrDrift, short(e.Expected), short(e.Actual))
}

// Is reports whether target is ErrDrift.
func (e *DriftError) Is(target error) bool { return target == ErrDrift }

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// Hash returns the hex sha256 of b.
func Hash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashFile returns the hash of the file at path, or NoFile when it does not
// exist.
func HashFile(path string) (string, error) {
	_, h, err := read(path)
	return h, err
}

// read returns the file's content and hash, with nil and NoFile when the
// file does not exist.
func read(path string) ([]byte, string, error) {
	b, err := safefs.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, NoFile, nil
	}
	if err != nil {
		return nil, "", err
	}
	return b, Hash(b), nil
}

// Plan reports what Apply would do without writing anything. It returns a
// *DriftError when Apply would refuse.
func Plan(p Patch) (Planned, error) {
	if p.ExpectedHash == "" {
		return Planned{}, fmt.Errorf("patch: %s: ExpectedHash is required (use NoFile for a new file)", p.File)
	}
	if p.kinds() != 1 {
		return Planned{}, fmt.Errorf("patch: %s: set exactly one of Transform, Link, and Remove", p.File)
	}
	file, err := filepath.Abs(p.File)
	if err != nil {
		return Planned{}, err
	}
	if p.Transform == nil {
		return planNoFollow(p, file)
	}
	cur, curHash, err := read(file)
	if err != nil {
		return Planned{}, err
	}
	next, err := p.Transform(cur)
	if err != nil {
		return Planned{}, fmt.Errorf("patch: %s: transform: %w", file, err)
	}
	// A repeat apply is recognized by the transform leaving its own output
	// unchanged, so a transform that does not is refused outright. Without
	// this, an appending transform re-applies on every run and a later
	// run misreports the patched file as drift.
	again, err := p.Transform(next)
	if err != nil {
		return Planned{}, fmt.Errorf("patch: %s: transform: %w", file, err)
	}
	if !bytes.Equal(again, next) {
		return Planned{}, fmt.Errorf("patch: %s: %w", file, ErrNotIdempotent)
	}
	pl := Planned{
		Record: Record{File: file, BeforeHash: curHash, AfterHash: Hash(next)},
		Before: cur,
		After:  next,
	}
	exists := curHash != NoFile
	if exists {
		fi, err := safefs.Stat(file)
		if err != nil {
			return Planned{}, err
		}
		pl.Mode = fi.Mode().Perm()
	} else {
		pl.Created = true
		pl.Mode = p.Mode
		if pl.Mode == 0 {
			pl.Mode = DefaultMode
		}
		// Known before anything is written, so a caller can record them
		// first and undo them even if Apply fails half way.
		if pl.CreatedDirs, err = missingDirs(filepath.Dir(file)); err != nil {
			return Planned{}, err
		}
	}
	same := exists && bytes.Equal(next, cur)
	switch {
	case same:
		// Either the expected file with an identity transform, or a file
		// this patch already changed. Both are a successful no-op.
		pl.Action = NoOp
	case curHash != p.ExpectedHash:
		return Planned{}, &DriftError{File: file, Expected: p.ExpectedHash, Actual: curHash}
	case !exists:
		pl.Action = Create
	default:
		pl.Action = Modify
	}
	return pl, nil
}

// planNoFollow plans a link or removal patch.
func planNoFollow(p Patch, file string) (Planned, error) {
	st, err := ReadState(file)
	if err != nil {
		return Planned{}, err
	}
	pl := Planned{Record: Record{File: file, BeforeHash: st.Hash, AfterHash: NoFile}, Before: st.Data, info: st.Info}
	if p.Link != "" {
		if err := safefs.CheckLinkTarget(file, p.Link); err != nil {
			return Planned{}, err
		}
		pl.AfterHash, pl.After = LinkHash(p.Link), []byte(p.Link)
	}
	if st.Info != nil && st.Info.Mode().IsRegular() {
		pl.Mode = st.Info.Mode().Perm()
	}
	switch {
	case st.Hash == pl.AfterHash:
		pl.Action = NoOp
	case st.Hash != p.ExpectedHash:
		return Planned{}, &DriftError{File: file, Expected: p.ExpectedHash, Actual: st.Hash}
	case p.Remove:
		pl.Action = Delete
	case st.Hash == NoFile:
		pl.Action, pl.Created = Create, true
		if pl.CreatedDirs, err = missingDirs(filepath.Dir(file)); err != nil {
			return Planned{}, err
		}
	default:
		pl.Action = Modify
	}
	return pl, nil
}

// writeFile is safefs.WriteFile; tests replace it to inject failures.
var writeFile = safefs.WriteFile

// ErrNotSynced matches an Apply or Revert error that means the target file
// was written but its directory could not be synced. The change happened.
// A sync failure on the backup is not reported: the backup exists, and the
// target write goes ahead.
var ErrNotSynced = errors.New("written but not synced")

// testHookBeforeWrite, set through SetTestHookBeforeWrite, runs right
// before Apply writes the target, after the re-check and the backup.
var testHookBeforeWrite func(file string) error

// SetTestHookBeforeWrite installs fn to run right before Apply writes its
// target; an error from fn fails the Apply there. It is for tests in
// other packages (a race window, a failure after MkdirAll), and panics
// outside a test binary. The returned func removes the hook.
func SetTestHookBeforeWrite(fn func(file string) error) (restore func()) {
	if !testing.Testing() {
		panic("patch: SetTestHookBeforeWrite outside a test")
	}
	old := testHookBeforeWrite
	testHookBeforeWrite = fn
	return func() { testHookBeforeWrite = old }
}

// Apply makes the change. When the file matches ExpectedHash it backs up the
// current bytes into backupDir and writes the transform's output atomically.
// When the file already holds the patched content it does nothing and
// succeeds. Otherwise it returns a *DriftError and writes nothing.
//
// When the new content is in place but syncing its directory fails, the
// change has happened and only its durability across a power loss is in
// doubt. Apply then returns the full Record together with an error matching
// ErrNotSynced (and safefs.ErrDirSync), and the caller must still record
// the change, or aggy unwire could not undo it. A sync failure on the
// backup is not an error: the backup exists. Every other error comes with
// a Record holding only File and the CreatedDirs Apply made itself before
// it failed, so the caller can record exactly those for removal.
//
// For a new file, CreatedDirs is the directories Apply made, which can be
// fewer than Plan listed: one that another process made in the meantime
// is not aggy's.
func Apply(p Patch, backupDir string) (Record, error) {
	pl, err := Plan(p)
	if err != nil {
		return Record{}, err
	}
	rec := pl.Record
	if pl.Action == NoOp {
		return rec, nil
	}
	if p.Transform == nil {
		return applyNoFollow(p, rec, backupDir)
	}
	if pl.Action == Modify && backupDir == "" {
		return Record{}, errors.New("patch: backup directory is required")
	}
	var made []string
	fail := func(err error) (Record, error) { return Record{File: rec.File, CreatedDirs: made}, err }
	if pl.Action == Create {
		if made, err = mkdirs(rec.CreatedDirs); err != nil {
			return fail(err)
		}
		rec.CreatedDirs = made
	}
	// Re-check right before writing to narrow the window for a concurrent
	// edit between the plan and the write.
	if h, err := HashFile(rec.File); err != nil {
		return fail(err)
	} else if h != rec.BeforeHash {
		return fail(&DriftError{File: rec.File, Expected: rec.BeforeHash, Actual: h})
	}
	// The backup is taken only once the write is going ahead, so a refused
	// patch leaves nothing behind.
	if pl.Action == Modify {
		if rec.BackupPath, err = backup(backupDir, pl.Before); err != nil {
			return fail(err)
		}
	}
	if testHookBeforeWrite != nil {
		if err := testHookBeforeWrite(rec.File); err != nil {
			return fail(err)
		}
	}
	if err := writeFile(rec.File, pl.After, rec.Mode); err != nil {
		if errors.Is(err, safefs.ErrDirSync) {
			rec.Changed = true
			return rec, fmt.Errorf("patch: %s: target %w: %w", rec.File, ErrNotSynced, err)
		}
		return fail(err)
	}
	rec.Changed = true
	return rec, nil
}

// applyNoFollow makes a planned link or removal change.
func applyNoFollow(p Patch, rec Record, backupDir string) (Record, error) {
	if rec.BeforeHash != NoFile && backupDir == "" {
		return Record{}, errors.New("patch: backup directory is required")
	}
	var made []string
	fail := func(err error) (Record, error) { return Record{File: rec.File, CreatedDirs: made}, err }
	if rec.Created {
		var err error
		if made, err = mkdirs(rec.CreatedDirs); err != nil {
			return fail(err)
		}
		rec.CreatedDirs = made
	}
	st, err := ReadState(rec.File)
	if err != nil {
		return fail(err)
	}
	if st.Hash != rec.BeforeHash {
		return fail(&DriftError{File: rec.File, Expected: rec.BeforeHash, Actual: st.Hash})
	}
	if rec.BeforeHash != NoFile {
		if rec.BackupPath, err = backupAs(backupDir, rec.BeforeHash, st.Data); err != nil {
			return fail(err)
		}
	}
	if testHookBeforeWrite != nil {
		if err := testHookBeforeWrite(rec.File); err != nil {
			return fail(err)
		}
	}
	if p.Remove {
		err = safefs.RemoveSame(rec.File, st.Info)
	} else {
		err = safefs.Symlink(p.Link, rec.File, st.Info)
	}
	if err != nil {
		if errors.Is(err, safefs.ErrDirSync) {
			rec.Changed = true
			return rec, fmt.Errorf("patch: %s: target %w: %w", rec.File, ErrNotSynced, err)
		}
		return fail(err)
	}
	rec.Changed = true
	return rec, nil
}

// testHookBeforeMkdir, set through SetTestHookBeforeMkdir, runs right
// before Apply makes the directories a new file needs.
var testHookBeforeMkdir func(dirs []string)

// SetTestHookBeforeMkdir installs fn to run right before Apply makes the
// missing directories it planned, to simulate another process making one
// first. It panics outside a test binary.
func SetTestHookBeforeMkdir(fn func(dirs []string)) (restore func()) {
	if !testing.Testing() {
		panic("patch: SetTestHookBeforeMkdir outside a test")
	}
	old := testHookBeforeMkdir
	testHookBeforeMkdir = fn
	return func() { testHookBeforeMkdir = old }
}

// mkdirs makes the planned missing directories, outermost first, one at
// a time, and returns those it made itself. One that exists by then was
// made by someone else and is not aggy's to remove, so it is left out.
func mkdirs(dirs []string) ([]string, error) {
	if testHookBeforeMkdir != nil {
		testHookBeforeMkdir(dirs)
	}
	var made []string
	for _, d := range dirs {
		err := safefs.Mkdir(d, 0o700)
		switch {
		case err == nil:
			made = append(made, d)
		case !errors.Is(err, fs.ErrExist):
			return made, err
		}
	}
	return made, nil
}

// missingDirs lists the directories that do not exist on the way to dir,
// outermost first.
func missingDirs(dir string) ([]string, error) {
	var missing []string
	for {
		_, err := safefs.Lstat(dir)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		missing = append([]string{dir}, missing...)
		up := filepath.Dir(dir)
		if up == dir {
			break
		}
		dir = up
	}
	return missing, nil
}

// backup stores b in dir under its own hash, so the same bytes are kept
// once and an existing backup is never overwritten with different content.
func backup(dir string, b []byte) (string, error) { return backupAs(dir, Hash(b), b) }

// backupAs stores b in dir as <name>.bak, where name is the state's hash:
// Hash(b) for a file, the LinkHash for a link's target b.
func backupAs(dir, name string, b []byte) (string, error) {
	if dir == "" {
		return "", errors.New("patch: backup directory is required")
	}
	if err := safefs.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".bak")
	if h, err := HashFile(path); err != nil {
		return "", err
	} else if h == Hash(b) {
		return path, nil
	}
	// A failed directory sync still leaves the backup in place and
	// readable, so it does not stop the change.
	if err := writeFile(path, b, 0o600); err != nil && !errors.Is(err, safefs.ErrDirSync) {
		return "", err
	}
	return path, nil
}

// Revert undoes an applied change. It restores the backup, or removes a file
// the patch created, only while the file still has AfterHash; otherwise it
// returns a *DriftError and leaves the file alone. Reverting a file that
// already has BeforeHash is a no-op, apart from removing the empty
// directories a created file needed. As with Apply, an error matching
// safefs.ErrDirSync means the file was restored but not synced.
//
// A link or removal change (Record.NoFollow) is compared and restored
// without following a symlink: the same link comes back, or the same
// file with its mode.
//
// Known limits of a content change, left for the T6607 follow-up: a file
// that was a symlink is restored through the link (the link stays and
// its target gets the old bytes), a hard link to the file is not kept
// (the restore writes a new inode), and extended attributes are not
// restored.
func Revert(r Record) error {
	if !r.Changed {
		return nil
	}
	_, cur, err := Current(r)
	if err != nil {
		return err
	}
	if cur == r.BeforeHash {
		// Already reverted, perhaps half way: finish removing the
		// directories a created file needed.
		if r.BeforeHash == NoFile {
			return removeEmptyDirs(r.CreatedDirs)
		}
		return nil
	}
	if cur != r.AfterHash {
		return &DriftError{File: r.File, Expected: r.AfterHash, Actual: cur}
	}
	return restore(r, cur)
}

// RevertForce undoes a change whatever the file holds now. The current
// bytes, when the file exists and differs from the before state, are
// first copied into backupDir under their hash, so a forced undo loses
// nothing; saved is that copy's path, or "" when nothing was saved. It
// is for an explicit, confirmed request only: Revert is the normal undo.
func RevertForce(r Record, backupDir string) (saved string, err error) {
	if !r.Changed {
		return "", nil
	}
	b, cur, err := Current(r)
	if err != nil {
		return "", err
	}
	if cur == r.BeforeHash {
		return "", Revert(r)
	}
	if cur != NoFile && cur != r.AfterHash {
		if saved, err = backupAs(backupDir, cur, b); err != nil {
			return "", err
		}
	}
	return saved, restore(r, cur)
}

// Target returns the content Revert would leave in r.File, and whether
// the file would exist at all. It reads and checks the backup.
func Target(r Record) ([]byte, bool, error) {
	if r.BeforeHash == NoFile {
		return nil, false, nil
	}
	old, err := safefs.ReadFile(r.BackupPath)
	if err != nil {
		return nil, false, fmt.Errorf("patch: read backup for %s: %w", r.File, err)
	}
	h := Hash(old)
	if IsLinkHash(r.BeforeHash) {
		h = LinkHash(string(old))
	}
	if h != r.BeforeHash {
		return nil, false, fmt.Errorf("patch: backup %s does not match the recorded hash", r.BackupPath)
	}
	return old, true, nil
}

// restore puts r's before state in place: it removes a created file, or
// writes the backup's bytes and the recorded mode. cur is the file's
// current hash.
func restore(r Record, cur string) error {
	if r.NoFollow() {
		return restoreNoFollow(r)
	}
	if r.BeforeHash == NoFile {
		if cur != NoFile {
			// TODO(T6607): exact restore also has to cover dangling
			// symlinks, hard links, and xattrs.
			if err := safefs.Remove(r.File); err != nil {
				return err
			}
		}
		return removeEmptyDirs(r.CreatedDirs)
	}
	old, _, err := Target(r)
	if err != nil {
		return err
	}
	mode := r.Mode
	if mode == 0 {
		mode = DefaultMode
	}
	if cur == NoFile {
		// Deleted since; only a forced undo gets here. Recreate its
		// directory if that went too.
		if err := safefs.MkdirAll(filepath.Dir(r.File), 0o700); err != nil {
			return err
		}
	}
	werr := writeFile(r.File, old, mode)
	if werr != nil && !errors.Is(werr, safefs.ErrDirSync) {
		return werr
	}
	if werr != nil {
		werr = fmt.Errorf("patch: %s: restored file %w: %w", r.File, ErrNotSynced, werr)
	}
	if r.Mode != 0 {
		fi, err := safefs.Stat(r.File)
		if err != nil {
			return err
		}
		if fi.Mode().Perm() != r.Mode {
			if err := safefs.Chmod(r.File, r.Mode); err != nil {
				return err
			}
		}
	}
	return werr
}

// restoreNoFollow puts back what a link or removal change found: nothing
// (removing the link it made), the same symlink, or the same file and
// mode. It replaces what is there now only while it keeps the identity it
// has as read here.
func restoreNoFollow(r Record) error {
	st, err := ReadState(r.File)
	if err != nil {
		return err
	}
	if r.BeforeHash == NoFile {
		if st.Hash != NoFile {
			if err := safefs.RemoveSame(r.File, st.Info); err != nil {
				return err
			}
		}
		return removeEmptyDirs(r.CreatedDirs)
	}
	old, _, err := Target(r)
	if err != nil {
		return err
	}
	if st.Hash == NoFile {
		if err := safefs.MkdirAll(filepath.Dir(r.File), 0o700); err != nil {
			return err
		}
	}
	if IsLinkHash(r.BeforeHash) {
		err = safefs.Symlink(string(old), r.File, st.Info)
	} else {
		mode := r.Mode
		if mode == 0 {
			mode = DefaultMode
		}
		err = safefs.WriteFileNoFollow(r.File, old, mode, st.Info)
	}
	if errors.Is(err, safefs.ErrDirSync) {
		return fmt.Errorf("patch: %s: restored %w: %w", r.File, ErrNotSynced, err)
	}
	return err
}

// RemoveEmptyDirs removes dirs, innermost first, while each is empty. A
// directory something else has put files in since is kept, and so are the
// ones above it. A directory that is gone already is skipped.
func RemoveEmptyDirs(dirs []string) error { return removeEmptyDirs(dirs) }

func removeEmptyDirs(dirs []string) error {
	for i := len(dirs) - 1; i >= 0; i-- {
		entries, err := safefs.ReadDir(dirs[i])
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return nil
		}
		if err := safefs.Remove(dirs[i]); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
