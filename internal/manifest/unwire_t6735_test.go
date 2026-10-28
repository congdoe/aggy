package manifest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
)

// failPending makes A: an Apply that fails before its write, then marks
// the row pending, as a process that stopped there would leave it.
func failPending(t *testing.T, f fixture, file string) store.Change {
	t.Helper()
	ctx := context.Background()
	restore := patch.SetTestHookBeforeWrite(func(string) error { return errors.New("boom") })
	_, err := f.m.Apply(ctx, "A", patch.Patch{File: file, ExpectedHash: patch.Hash([]byte("X\n")), Transform: appendText("A\n")})
	restore()
	if err == nil {
		t.Fatal("A did not fail")
	}
	cs, _ := f.s.Changes(ctx, "A")
	if _, err := f.s.DB().ExecContext(ctx, `UPDATE changes SET status = 'pending' WHERE change_id = ?`, cs[0].ChangeID); err != nil {
		t.Fatal(err)
	}
	return cs[0]
}

// TestUnwireUnlockedPlanProvesNothing (T6735 1): a newer change planned
// on A's hash before proves A never wrote only when it held the target
// lock. Planned unlocked, it is recorded so, and A waits (Blocked)
// instead of being Superseded. Fails on 7a6c6a4.
func TestUnwireUnlockedPlanProvesNothing(t *testing.T) {
	for _, unlocked := range []bool{false, true} {
		t.Run(map[bool]string{false: "locked", true: "unlocked"}[unlocked], func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			file := filepath.Join(f.dir, "c.toml")
			must(t, os.WriteFile(file, []byte("X\n"), 0o600))
			a := failPending(t, f, file)
			if unlocked {
				real := lockFile
				lockFile = func(context.Context, string, time.Duration) (func(), error) {
					return func() {}, store.ErrLockUnsupported
				}
				t.Cleanup(func() { lockFile = real })
			}
			b := apply(t, f, "B", file, "B\n")
			if b.Unlocked != unlocked {
				t.Errorf("B.Unlocked = %v, want %v", b.Unlocked, unlocked)
			}
			undos, _ := f.m.Unwire(ctx, Selection{RunID: "A"}, UnwireOptions{}, io.Discard)
			want := Superseded
			if unlocked {
				want = Blocked
			}
			if got := outcomeOf(undos, a.ChangeID); got != want {
				t.Errorf("A = %q, want %q", got, want)
			}
		})
	}
}

// TestUnwireFailedRowRewrittenBirth (T6735 2): APFS rewrote the birth
// time of the directory a failed create made. Its device and inode still
// match, and this is the undo of the change that made it, so the empty
// directory goes. Fails on 7a6c6a4.
func TestUnwireFailedRowRewrittenBirth(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	real := dirIdentity
	shift := int64(0)
	dirIdentity = func(p string) (store.DirID, bool) {
		id, ok := real(p)
		if ok {
			id.Birth = 100 + shift
		}
		return id, ok
	}
	t.Cleanup(func() { dirIdentity = real })
	dir := filepath.Join(f.dir, "made")
	restore := patch.SetTestHookBeforeWrite(func(string) error { return errors.New("boom") })
	_, err := f.m.Apply(ctx, "A", patch.Patch{File: filepath.Join(dir, "x.toml"), ExpectedHash: patch.NoFile, Transform: appendText("x\n")})
	restore()
	if err == nil {
		t.Fatal("A did not fail")
	}
	shift = 7 // a restore rewrote the birth time
	if _, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); err == nil {
		t.Error("the directory the failed create made is still there")
	}
}

// TestUnwirePendingAtHashBeforeIsCleaned (T6735 3): with the pending
// branch of provenUnwritten gone, a pending row whose file is still at
// its hash before and whose backup is missing is Cleaned by the normal
// path, file untouched. (Behavior kept; the branch was unreachable in
// effect.)
func TestUnwirePendingAtHashBeforeIsCleaned(t *testing.T) {
	for _, dropAll := range []bool{false, true} {
		f := setup(t)
		ctx := context.Background()
		file := filepath.Join(f.dir, "c.toml")
		must(t, os.WriteFile(file, []byte("X\n"), 0o640))
		a := failPending(t, f, file)
		must(t, os.RemoveAll(filepath.Join(f.m.backupRoot, a.ChangeID)))
		if dropAll {
			must(t, os.RemoveAll(f.m.backupRoot))
		}
		undos, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard)
		if err != nil || outcomeOf(undos, a.ChangeID) != Cleaned {
			t.Errorf("dropAll=%v: %+v, %v, want cleaned", dropAll, undos, err)
		}
		if b, _ := os.ReadFile(file); string(b) != "X\n" {
			t.Errorf("file = %q", b)
		}
	}
}

// TestFailedCreateRecordsOnlyItsDirs (T6735 4a): another process makes
// the outer directory between the plan and the mkdir. The failed row
// records only the inner one, which aggy made, and unwire keeps the
// outer one. Fails on 7a6c6a4.
func TestFailedCreateRecordsOnlyItsDirs(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	outer := filepath.Join(f.dir, "outer")
	inner := filepath.Join(outer, "inner")
	defer patch.SetTestHookBeforeMkdir(func([]string) { must(t, os.Mkdir(outer, 0o755)) })()
	defer patch.SetTestHookBeforeWrite(func(string) error { return errors.New("boom") })()
	_, err := f.m.Apply(ctx, "A", patch.Patch{File: filepath.Join(inner, "x.toml"), ExpectedHash: patch.NoFile, Transform: appendText("x\n")})
	if err == nil {
		t.Fatal("A did not fail")
	}
	cs, _ := f.s.Changes(ctx, "A")
	if len(cs) != 1 || len(cs[0].CreatedDirs) != 1 || cs[0].CreatedDirs[0] != inner {
		t.Fatalf("failed row dirs = %+v", cs)
	}
	if _, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(inner); err == nil {
		t.Error("aggy's directory is still there")
	}
	if _, err := os.Lstat(outer); err != nil {
		t.Errorf("unwire removed the other process's directory: %v", err)
	}
}

// TestFailedRowWithoutIDsKeepsDirs (T6735 4b): a failed row that recorded
// directories but no identity for them does not remove a directory by
// path alone; a pending row, which could not record any, still does.
// Fails on 7a6c6a4.
func TestFailedRowWithoutIDsKeepsDirs(t *testing.T) {
	for _, st := range []string{store.ChangeFailed, store.ChangePending} {
		t.Run(st, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			dir := filepath.Join(f.dir, "d")
			must(t, os.Mkdir(dir, 0o755))
			now := time.Now()
			must(t, f.s.InsertChange(ctx, store.Change{
				ChangeID: "20260101T000000-aaaaaaaaaaaa", RunID: "A", File: filepath.Join(dir, "x.toml"),
				HashBefore: patch.NoFile, HashAfter: patch.Hash([]byte("x\n")), CreatedFile: true,
				CreatedDirs: []string{dir}, Status: st, PlannedAt: now,
			}))
			if _, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard); err != nil {
				t.Fatal(err)
			}
			_, err := os.Lstat(dir)
			if kept := err == nil; kept != (st == store.ChangeFailed) {
				t.Errorf("%s row: dir kept = %v", st, kept)
			}
		})
	}
}
