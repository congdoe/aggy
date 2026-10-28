package manifest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
)

// Regression tests for the second PR #6 review. N1 and N11 fail on
// 12a6a3d, where the superseded rule and the sweep came in; N2 and the
// home check fail there too.

// pendingWritten applies text to file in run, then sets the row back to
// pending with no backup path, as a stop between the write and the
// record leaves it.
func pendingWritten(t *testing.T, f fixture, run, file, text string) store.Change {
	t.Helper()
	c := apply(t, f, run, file, text)
	if _, err := f.s.DB().ExecContext(context.Background(),
		`UPDATE changes SET status = 'pending', backup_path = '' WHERE change_id = ?`, c.ChangeID); err != nil {
		t.Fatal(err)
	}
	return c
}

func outcomeOf(undos []Undo, id string) Outcome {
	for _, u := range undos {
		if u.Change.ChangeID == id {
			return u.Outcome
		}
	}
	return ""
}

// TestUnwirePendingUnderNewerApplied (N1, scenario A): A wrote and stayed
// pending, B was planned on A's result and applied. --run A must refuse;
// --all must end at the original bytes.
func TestUnwirePendingUnderNewerApplied(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "c.toml")
	must(t, os.WriteFile(file, []byte("X\n"), 0o600))
	a := pendingWritten(t, f, "A", file, "A\n")
	apply(t, f, "B", file, "B\n")
	undos, err := f.m.Unwire(ctx, Selection{RunID: "A"}, UnwireOptions{}, io.Discard)
	if !errors.Is(err, ErrNewerChange) || outcomeOf(undos, a.ChangeID) != Blocked {
		t.Errorf("--run A = %+v, %v, want Blocked", undos, err)
	}
	if b, _ := os.ReadFile(file); string(b) != "X\nA\nB\n" {
		t.Errorf("--run A changed the file: %q", b)
	}
	if s := status(t, f, a.ChangeID); s != store.ChangePending {
		t.Errorf("A = %s, want still pending", s)
	}
	if _, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != "X\n" {
		t.Errorf("after --all = %q, want %q", b, "X\n")
	}
}

// TestUnwirePendingUnderNewerFailed (N1, scenario B): as A, but B failed
// without writing.
func TestUnwirePendingUnderNewerFailed(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "c.toml")
	must(t, os.WriteFile(file, []byte("X\n"), 0o600))
	a := pendingWritten(t, f, "A", file, "A\n")
	restore := patch.SetTestHookBeforeWrite(func(string) error { return errors.New("boom") })
	h, _ := patch.HashFile(file)
	_, err := f.m.Apply(ctx, "B", patch.Patch{File: file, ExpectedHash: h, Transform: appendText("B\n")})
	restore()
	if err == nil {
		t.Fatal("B did not fail")
	}
	undos, err := f.m.Unwire(ctx, Selection{RunID: "A"}, UnwireOptions{}, io.Discard)
	if !errors.Is(err, ErrNewerChange) || outcomeOf(undos, a.ChangeID) != Blocked {
		t.Errorf("--run A = %+v, %v, want Blocked", undos, err)
	}
	if b, _ := os.ReadFile(file); string(b) != "X\nA\n" {
		t.Errorf("--run A changed the file: %q", b)
	}
	if _, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != "X\n" {
		t.Errorf("after --all = %q, want %q", b, "X\n")
	}
}

// TestUnwireBackupRootSpelledDifferently (N2, scenario 1): the state
// directory reached through another spelling (here a symlink, as
// /tmp and /private/tmp on macOS) still undoes the change, forced too.
func TestUnwireBackupRootSpelledDifferently(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	link := filepath.Join(t.TempDir(), "state-link")
	must(t, os.Symlink(f.state, link))
	for _, force := range []bool{false, true} {
		file := filepath.Join(f.dir, "c.toml")
		must(t, os.WriteFile(file, []byte("orig\n"), 0o640))
		c := apply(t, f, "r", file, "aggy\n")
		if force {
			must(t, os.WriteFile(file, []byte("orig\naggy\nmine\n"), 0o640))
		}
		m2 := New(f.s, filepath.Join(link, BackupDirName))
		undos, err := m2.Unwire(ctx, Selection{ChangeID: c.ChangeID}, UnwireOptions{Force: force}, io.Discard)
		if err != nil || outcomeOf(undos, c.ChangeID) != Restored {
			t.Fatalf("force=%v: Unwire = %+v, %v", force, undos, err)
		}
		if b, _ := os.ReadFile(file); string(b) != "orig\n" {
			t.Errorf("force=%v: file = %q", force, b)
		}
	}
}

// TestUnwireStateDirMovedAndLinked (N2, scenario 2): ~/.aggy moved into
// a dotfiles directory and linked back; the backups are read from where
// they are now.
func TestUnwireStateDirMovedAndLinked(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := t.TempDir()
	state := filepath.Join(base, "state")
	must(t, os.MkdirAll(state, 0o700))
	t.Setenv("AGGY_HOME", state)
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	must(t, err)
	m, err := Open(s)
	must(t, err)
	dir := t.TempDir()
	file := filepath.Join(dir, "c.toml")
	must(t, os.WriteFile(file, []byte("orig\n"), 0o600))
	f := fixture{m: m, s: s, state: state, dir: dir}
	c := apply(t, f, "r", file, "aggy\n")
	must(t, s.Close())

	moved := filepath.Join(base, "dotfiles", "aggy")
	must(t, os.MkdirAll(filepath.Dir(moved), 0o700))
	must(t, os.Rename(state, moved))
	must(t, os.Symlink(moved, state))
	s, err = store.OpenDefault(ctx)
	must(t, err)
	defer func() { _ = s.Close() }()
	m, err = Open(s)
	must(t, err)
	undos, err := m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard)
	if err != nil || outcomeOf(undos, c.ChangeID) != Restored {
		t.Fatalf("Unwire = %+v, %v", undos, err)
	}
	if b, _ := os.ReadFile(file); string(b) != "orig\n" {
		t.Errorf("file = %q", b)
	}
}

// TestUnwireRefusesHomeAsCreatedDir (N5): a row may not claim to have
// created the home directory, or anything above it.
func TestUnwireRefusesHomeAsCreatedDir(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	h := os.Getenv("HOME")
	file := filepath.Join(h, "x.toml")
	must(t, os.WriteFile(file, []byte("x\n"), 0o600))
	insertRow(t, f, store.Change{ChangeID: "c1", File: file, HashBefore: patch.NoFile,
		HashAfter: patch.Hash([]byte("x\n")), CreatedFile: true, CreatedDirs: []string{filepath.Dir(h), h}})
	undos, _ := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard)
	if outcomeOf(undos, "c1") != Invalid {
		t.Errorf("Unwire = %+v, want Invalid", undos)
	}
	if _, err := os.Lstat(file); err != nil {
		t.Errorf("file removed: %v", err)
	}
}

// TestSweepKeepsDirMadeLater (N11): a directory aggy created and removed,
// made again later by someone else at the same path, is not swept.
func TestSweepKeepsDirMadeLater(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	dir := filepath.Join(f.dir, "made")
	c := apply(t, f, "R1", filepath.Join(dir, "x.toml"), "x\n")
	got, _ := f.s.GetChange(ctx, c.ChangeID)
	if len(got.CreatedDirIDs) != 1 || got.CreatedDirIDs[0] == (store.DirID{}) {
		t.Errorf("created dir identity not recorded: %+v", got.CreatedDirIDs)
	}
	if _, err := f.m.Unwire(ctx, Selection{RunID: "R1"}, UnwireOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	must(t, os.Mkdir(dir, 0o755)) // the person's own directory now
	apply(t, f, "R2", filepath.Join(f.dir, "other.toml"), "y\n")
	if _, err := f.m.Unwire(ctx, Selection{RunID: "R2"}, UnwireOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Errorf("the sweep removed a directory made later: %v", err)
	}
}

// TestCreatedDirsOldFormat: rows written as a plain list of paths still
// read.
func TestCreatedDirsOldFormat(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	insertRow(t, f, store.Change{ChangeID: "c1", File: "/a/b/c", HashBefore: patch.NoFile,
		HashAfter: patch.Hash([]byte("x")), CreatedFile: true})
	if _, err := f.s.DB().ExecContext(ctx, `UPDATE changes SET created_dirs = '["/a/b"]' WHERE change_id = 'c1'`); err != nil {
		t.Fatal(err)
	}
	c, err := f.s.GetChange(ctx, "c1")
	if err != nil || len(c.CreatedDirs) != 1 || c.CreatedDirs[0] != "/a/b" || c.CreatedDirIDs != nil {
		t.Errorf("old format = %+v, %v", c, err)
	}
}
