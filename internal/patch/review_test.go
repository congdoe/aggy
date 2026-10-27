package patch

import (
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/congdoe/aggy/internal/safefs"
)

// Regression tests for the PR #2 review.

// appendLine changes its own output every time: not idempotent.
func appendLine(b []byte) ([]byte, error) {
	return append(append([]byte{}, b...), "x = 1\n"...), nil
}

// TestNotIdempotentRefused: an appending transform used to re-apply on
// every run, and a run with the original hash then reported drift.
func TestNotIdempotentRefused(t *testing.T) {
	orig := "a = 1\n"
	e := setup(t, ptr(orig))
	p := Patch{File: e.file, ExpectedHash: Hash([]byte(orig)), Transform: appendLine}

	if _, err := Plan(p); !errors.Is(err, ErrNotIdempotent) {
		t.Errorf("Plan = %v, want ErrNotIdempotent", err)
	}
	if _, err := Apply(p, e.backups); !errors.Is(err, ErrNotIdempotent) {
		t.Fatalf("Apply = %v, want ErrNotIdempotent", err)
	}
	if errors.Is(ErrNotIdempotent, ErrDrift) {
		t.Error("ErrNotIdempotent must be distinct from ErrDrift")
	}
	if got := content(t, e.file); got != orig {
		t.Errorf("file written: %q", got)
	}
	if _, err := os.Stat(e.backups); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("backup taken for a refused patch: %v", err)
	}

	// A new file with a non-idempotent transform is refused too.
	q := Patch{File: e.file + ".new", ExpectedHash: NoFile, Transform: appendLine}
	if _, err := Apply(q, e.backups); !errors.Is(err, ErrNotIdempotent) {
		t.Errorf("Apply new = %v, want ErrNotIdempotent", err)
	}
}

// TestReapplyIsAlreadyAppliedNotDrift: re-running a successful patch with
// the hash it was planned against is a no-op, never drift.
func TestReapplyIsAlreadyAppliedNotDrift(t *testing.T) {
	orig := "a = 1\n"
	e := setup(t, ptr(orig))
	p := Patch{File: e.file, ExpectedHash: Hash([]byte(orig)), Transform: addLine}
	for i := range 3 {
		rec, err := Apply(p, e.backups)
		if err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		if rec.Changed != (i == 0) {
			t.Errorf("apply %d: Changed = %v", i, rec.Changed)
		}
		pl, err := Plan(p)
		if err != nil || pl.Action != NoOp {
			t.Errorf("plan after apply %d = %s, %v, want noop", i, pl.Action, err)
		}
	}
}

// TestNoBackupWhenRecheckFails: the backup used to be written before the
// final hash re-check, so a refused patch left a backup behind.
func TestNoBackupWhenRecheckFails(t *testing.T) {
	orig := "a = 1\n"
	e := setup(t, ptr(orig))
	calls := 0
	// The transform edits the file on its first call, standing in for a
	// concurrent edit between Plan and the write.
	racy := func(b []byte) ([]byte, error) {
		calls++
		if calls == 1 {
			if err := os.WriteFile(e.file, []byte("edited\n"), 0o640); err != nil {
				return nil, err
			}
		}
		return addLine(b)
	}
	_, err := Apply(Patch{File: e.file, ExpectedHash: Hash([]byte(orig)), Transform: racy}, e.backups)
	if !errors.Is(err, ErrDrift) {
		t.Fatalf("Apply = %v, want ErrDrift", err)
	}
	if got := content(t, e.file); got != "edited\n" {
		t.Errorf("file = %q, want the concurrent edit kept", got)
	}
	entries, _ := os.ReadDir(e.backups)
	if len(entries) != 0 {
		t.Errorf("backup left after a refused patch: %v", entries)
	}
}

// TestApplyKeepsRecordOnDirSyncError: a directory sync that fails after the
// rename must not lose the Record, or the manifest could not undo a change
// that happened.
func TestApplyKeepsRecordOnDirSyncError(t *testing.T) {
	orig := "a = 1\n"
	e := setup(t, ptr(orig))
	old := writeFile
	writeFile = func(name string, data []byte, perm fs.FileMode) error {
		if err := old(name, data, perm); err != nil {
			return err
		}
		return &safefs.DirSyncError{Path: name, Err: errors.New("EIO")}
	}
	t.Cleanup(func() { writeFile = old })

	rec, err := Apply(Patch{File: e.file, ExpectedHash: Hash([]byte(orig)), Transform: addLine}, e.backups)
	if !errors.Is(err, safefs.ErrDirSync) {
		t.Fatalf("Apply = %v, want ErrDirSync", err)
	}
	if !rec.Changed || rec.BackupPath == "" || rec.AfterHash != Hash([]byte(orig+line)) || rec.BeforeHash != Hash([]byte(orig)) {
		t.Fatalf("Record lost on a dir sync error: %+v", rec)
	}
	if got := content(t, e.file); got != orig+line {
		t.Errorf("file = %q", got)
	}
	// The Record is complete enough to undo the change.
	writeFile = old
	if err := Revert(rec); err != nil {
		t.Fatal(err)
	}
	if got := content(t, e.file); got != orig {
		t.Errorf("after revert = %q, want %q", got, orig)
	}
}

// TestCreateRecordsUndoFacts: a created file records that it was created,
// its mode, and the directories made for it, and Revert removes them.
func TestCreateRecordsUndoFacts(t *testing.T) {
	e := setup(t, nil)
	file := e.dir + "/a/b/new.toml"
	rec, err := Apply(Patch{File: file, ExpectedHash: NoFile, Transform: addLine, Mode: 0o640}, e.backups)
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Created || rec.Mode != 0o640 {
		t.Errorf("Created, Mode = %v, %o", rec.Created, rec.Mode)
	}
	if want := []string{e.dir + "/a", e.dir + "/a/b"}; len(rec.CreatedDirs) != 2 || rec.CreatedDirs[0] != want[0] || rec.CreatedDirs[1] != want[1] {
		t.Errorf("CreatedDirs = %v, want %v", rec.CreatedDirs, want)
	}
	if err := Revert(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.dir + "/a"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("created directory left after revert: %v", err)
	}
}

// TestRevertKeepsDirsWithOtherFiles: a created directory that gained
// another file is kept.
func TestRevertKeepsDirsWithOtherFiles(t *testing.T) {
	e := setup(t, nil)
	file := e.dir + "/a/b/new.toml"
	rec, err := Apply(Patch{File: file, ExpectedHash: NoFile, Transform: addLine}, e.backups)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.dir+"/a/other", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Revert(rec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.dir + "/a/b"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("empty created directory kept: %v", err)
	}
	if _, err := os.Stat(e.dir + "/a/other"); err != nil {
		t.Errorf("directory with another file removed: %v", err)
	}
}

// TestRevertRestoresMode: the mode recorded before the change comes back.
func TestRevertRestoresMode(t *testing.T) {
	orig := "a = 1\n"
	e := setup(t, ptr(orig))
	rec, err := Apply(Patch{File: e.file, ExpectedHash: Hash([]byte(orig)), Transform: addLine}, e.backups)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Mode != 0o640 || rec.Created {
		t.Errorf("Mode, Created = %o, %v", rec.Mode, rec.Created)
	}
	if err := os.Chmod(e.file, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Revert(rec); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(e.file)
	if err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("mode after revert = %v, %v, want 640", fi.Mode().Perm(), err)
	}
}
