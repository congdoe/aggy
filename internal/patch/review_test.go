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
