// Package patch makes guarded, reversible changes to config files. A Patch
// names a file, the hash it expects to find there, and a transform. Apply
// writes only when the file still matches, backs up the old bytes, and
// returns a Record that aggy unwire can use to restore them byte for byte.
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
}

// Record describes an applied change in the shape of the store's changes
// table: file, hash before, hash after, backup path.
type Record struct {
	File       string // absolute path
	BeforeHash string // hex sha256, or NoFile when the patch created the file
	AfterHash  string // hex sha256 after the change
	BackupPath string // copy of the old bytes; empty when BeforeHash is NoFile or nothing changed
	Changed    bool   // false when the file already held the patched content
}

// Action is what applying a patch would do.
type Action string

// Actions reported by Plan.
const (
	Create Action = "create" // the file does not exist and would be created
	Modify Action = "modify" // the file would be rewritten
	NoOp   Action = "noop"   // the file already holds the patched content
)

// Planned is the dry-run result of a patch.
type Planned struct {
	Record
	Action Action
	Before []byte // current content, nil when the file does not exist
	After  []byte // content after the change
}

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
	if p.Transform == nil {
		return Planned{}, fmt.Errorf("patch: %s: Transform is required", p.File)
	}
	file, err := filepath.Abs(p.File)
	if err != nil {
		return Planned{}, err
	}
	cur, curHash, err := read(file)
	if err != nil {
		return Planned{}, err
	}
	next, err := p.Transform(cur)
	if err != nil {
		return Planned{}, fmt.Errorf("patch: %s: transform: %w", file, err)
	}
	pl := Planned{
		Record: Record{File: file, BeforeHash: curHash, AfterHash: Hash(next)},
		Before: cur,
		After:  next,
	}
	exists := curHash != NoFile
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

// Apply makes the change. When the file matches ExpectedHash it backs up the
// current bytes into backupDir and writes the transform's output atomically.
// When the file already holds the patched content it does nothing and
// succeeds. Otherwise it returns a *DriftError and writes nothing.
func Apply(p Patch, backupDir string) (Record, error) {
	pl, err := Plan(p)
	if err != nil {
		return Record{}, err
	}
	rec := pl.Record
	if pl.Action == NoOp {
		return rec, nil
	}
	if pl.Action == Modify {
		if rec.BackupPath, err = backup(backupDir, pl.Before); err != nil {
			return Record{}, err
		}
	}
	mode := p.Mode
	if mode == 0 {
		mode = DefaultMode
	}
	if pl.Action == Create {
		if err := safefs.MkdirAll(filepath.Dir(rec.File), 0o700); err != nil {
			return Record{}, err
		}
	}
	// Re-check right before writing to narrow the window for a concurrent
	// edit between the plan and the write.
	if h, err := HashFile(rec.File); err != nil {
		return Record{}, err
	} else if h != rec.BeforeHash {
		return Record{}, &DriftError{File: rec.File, Expected: rec.BeforeHash, Actual: h}
	}
	if err := safefs.WriteFile(rec.File, pl.After, mode); err != nil {
		return Record{}, err
	}
	rec.Changed = true
	return rec, nil
}

// backup stores b in dir under its own hash, so the same bytes are kept
// once and an existing backup is never overwritten with different content.
func backup(dir string, b []byte) (string, error) {
	if dir == "" {
		return "", errors.New("patch: backup directory is required")
	}
	if err := safefs.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, Hash(b)+".bak")
	if h, err := HashFile(path); err != nil {
		return "", err
	} else if h == Hash(b) {
		return path, nil
	}
	if err := safefs.WriteFile(path, b, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// Revert undoes an applied change. It restores the backup, or removes a file
// the patch created, only while the file still has AfterHash; otherwise it
// returns a *DriftError and leaves the file alone. Reverting a file that
// already has BeforeHash is a no-op.
func Revert(r Record) error {
	if !r.Changed {
		return nil
	}
	cur, err := HashFile(r.File)
	if err != nil {
		return err
	}
	if cur == r.BeforeHash {
		return nil
	}
	if cur != r.AfterHash {
		return &DriftError{File: r.File, Expected: r.AfterHash, Actual: cur}
	}
	if r.BeforeHash == NoFile {
		return safefs.Remove(r.File)
	}
	old, err := safefs.ReadFile(r.BackupPath)
	if err != nil {
		return fmt.Errorf("patch: read backup for %s: %w", r.File, err)
	}
	if Hash(old) != r.BeforeHash {
		return fmt.Errorf("patch: backup %s does not match the recorded hash", r.BackupPath)
	}
	return safefs.WriteFile(r.File, old, DefaultMode)
}
