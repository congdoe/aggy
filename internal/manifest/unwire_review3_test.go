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

// Regression tests for T6733. TestUnwireWrittenPendingUnderHandEdit
// fails on 45fa3fa, where a pending row with any newer change in place
// was superseded whether or not its write had happened.

// TestUnwireWrittenPendingUnderHandEdit: A wrote and stayed pending, the
// person edited the file, and B was planned on that edit and applied. A
// is not proven unwritten (B's hash before is not A's), so it is never
// superseded: it waits for B, and once B is undone it is Drifted, still
// pending, with its line in the file for the person to see.
func TestUnwireWrittenPendingUnderHandEdit(t *testing.T) {
	type step struct {
		sel     func(a, b store.Change) Selection
		outcome Outcome // A's outcome at this step
	}
	tests := []struct {
		name      string
		editAfter bool // the person edits again after B, so B drifts
		steps     []step
	}{
		{"run A then all", false, []step{
			{func(a, _ store.Change) Selection { return Selection{RunID: a.RunID} }, Blocked},
			{func(_, _ store.Change) Selection { return Selection{All: true} }, Drifted},
		}},
		{"change A then all", false, []step{
			{func(a, _ store.Change) Selection { return Selection{ChangeID: a.ChangeID} }, Blocked},
			{func(_, _ store.Change) Selection { return Selection{All: true} }, Drifted},
		}},
		{"change B then all", false, []step{
			{func(_, b store.Change) Selection { return Selection{ChangeID: b.ChangeID} }, ""},
			{func(_, _ store.Change) Selection { return Selection{All: true} }, Drifted},
		}},
		{"all", false, []step{
			{func(_, _ store.Change) Selection { return Selection{All: true} }, Drifted},
		}},
		{"all with B drifted", true, []step{
			{func(_, _ store.Change) Selection { return Selection{All: true} }, Blocked},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			file := filepath.Join(f.dir, "c.toml")
			must(t, os.WriteFile(file, []byte("X\n"), 0o600))
			a := pendingWritten(t, f, "A", file, "A\n")
			must(t, os.WriteFile(file, []byte("X\nA\nuser\n"), 0o600))
			b := apply(t, f, "B", file, "B\n")
			if tt.editAfter {
				must(t, os.WriteFile(file, []byte("X\nA\nuser\nB\nmore\n"), 0o600))
			}
			for i, st := range tt.steps {
				undos, _ := f.m.Unwire(ctx, st.sel(a, b), UnwireOptions{}, io.Discard)
				if got := outcomeOf(undos, a.ChangeID); got != st.outcome {
					t.Errorf("step %d: A = %q, want %q (%+v)", i, got, st.outcome, undos)
				}
			}
			if s := status(t, f, a.ChangeID); s != store.ChangePending {
				t.Errorf("A = %s, want still pending: its line is in the file", s)
			}
			// The person can still take A out, once B is gone.
			if !tt.editAfter {
				undos, err := f.m.Unwire(ctx, Selection{ChangeID: a.ChangeID}, UnwireOptions{Force: true}, io.Discard)
				if err != nil || outcomeOf(undos, a.ChangeID) != Restored {
					t.Fatalf("forced A = %+v, %v", undos, err)
				}
				if got, _ := os.ReadFile(file); string(got) != "X\n" {
					t.Errorf("after forcing A = %q, want %q", got, "X\n")
				}
			}
		})
	}
}

// TestUnwireSupersededNeedsSameHashBefore: a pending or failed row is
// superseded only when a newer change to the file was planned on its
// hash before.
func TestUnwireSupersededNeedsSameHashBefore(t *testing.T) {
	tests := []struct {
		name  string
		edit  string // what the person writes between A and B; "" for none
		wrote bool   // A's write happened
		want  Outcome
	}{
		{"A never wrote, B on the same bytes", "", false, Superseded},
		{"A never wrote, B on a hand edit", "X\nuser\n", false, Blocked},
		{"A wrote, B on a hand edit of it", "X\nA\nuser\n", true, Blocked},
		{"A wrote, undone by hand, B on the old bytes", "X\n", true, Superseded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			file := filepath.Join(f.dir, "c.toml")
			must(t, os.WriteFile(file, []byte("X\n"), 0o600))
			var a store.Change
			if tt.wrote {
				a = pendingWritten(t, f, "A", file, "A\n")
			} else {
				restore := patch.SetTestHookBeforeWrite(func(string) error { return errors.New("boom") })
				_, err := f.m.Apply(ctx, "A", patch.Patch{File: file, ExpectedHash: patch.Hash([]byte("X\n")), Transform: appendText("A\n")})
				restore()
				if err == nil {
					t.Fatal("A did not fail")
				}
				cs, _ := f.s.Changes(ctx, "A")
				a = cs[0]
				// Pending with its backup made: nothing proves it never
				// wrote but a newer change (a failed row is proven by its
				// status alone and is Cleaned).
				if _, err := f.s.DB().ExecContext(ctx, `UPDATE changes SET status = 'pending' WHERE change_id = ?`, a.ChangeID); err != nil {
					t.Fatal(err)
				}
			}
			if tt.edit != "" {
				must(t, os.WriteFile(file, []byte(tt.edit), 0o600))
			}
			apply(t, f, "B", file, "B\n")
			before, _ := os.ReadFile(file)
			undos, _ := f.m.Unwire(ctx, Selection{RunID: "A"}, UnwireOptions{}, io.Discard)
			if got := outcomeOf(undos, a.ChangeID); got != tt.want {
				t.Errorf("A = %q, want %q", got, tt.want)
			}
			if got, _ := os.ReadFile(file); string(got) != string(before) {
				t.Errorf("undoing run A changed the file: %q", got)
			}
		})
	}
}

// noBirth makes dirIdentity report no birth times, as on Linux, for the
// rest of the test.
func noBirth(t *testing.T) {
	real := dirIdentity
	dirIdentity = func(p string) (store.DirID, bool) {
		id, ok := real(p)
		id.Birth = 0
		return id, ok
	}
	t.Cleanup(func() { dirIdentity = real })
}

// TestSweepSkipsUnrelatedRun: R1 made a directory and R2 put a second
// file in it. After R1 is undone and the person deletes R2's file by
// hand, the directory looks, by device and inode alone (no birth time,
// as on Linux), like the one aggy made, as one the person made again
// would on ext4. An unwire unrelated to it must not sweep it. Nor does
// R2's own undo since T6735: it finds y.toml already gone, which does not
// relate it to the directory (the person may have made it again), so
// without birth times the empty directory stays.
func TestSweepSkipsUnrelatedRun(t *testing.T) {
	f := setup(t)
	noBirth(t)
	ctx := context.Background()
	dir := filepath.Join(f.dir, "made")
	apply(t, f, "R1", filepath.Join(dir, "x.toml"), "x\n")
	apply(t, f, "R2", filepath.Join(dir, "y.toml"), "y\n")
	apply(t, f, "R3", filepath.Join(f.dir, "other.toml"), "z\n")
	if _, err := f.m.Unwire(ctx, Selection{RunID: "R1"}, UnwireOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	must(t, os.Remove(filepath.Join(dir, "y.toml")))
	if _, err := f.m.Unwire(ctx, Selection{RunID: "R3"}, UnwireOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatalf("an unrelated unwire swept the directory: %v", err)
	}
	if _, err := f.m.Unwire(ctx, Selection{RunID: "R2"}, UnwireOptions{}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Errorf("R2's undo, finding its file already gone, swept the directory: %v", err)
	}
}

// TestSweepInodeReuse simulates ext4 handing aggy's freed inode number to
// a directory the person made again at the same path. Birth times, where
// recorded, tell the two apart; where there are none, only an unwire that
// removes a file of aggy's from the directory may sweep it.
func TestSweepInodeReuse(t *testing.T) {
	tests := []struct {
		name        string
		birth       bool   // the file system reports birth times
		putBack     bool   // the person put R2's file back in the new directory
		run         string // the run unwound after the directory is made again
		wantRemoved bool
	}{
		{"no birth time, unrelated unwire", false, false, "R3", false},
		{"birth time, unrelated unwire", true, false, "R3", false},
		{"birth time, related unwire", true, true, "R2", false},
		{"birth time, related unwire, file gone", true, false, "R2", false},
		// The documented limit: without birth times, the unwire of
		// aggy's file in a directory made again around it takes the
		// directory when that leaves it empty.
		{"no birth time, related unwire", false, true, "R2", true},
		// T6735: a file found already gone relates the unwire to
		// nothing, so the directory made again is kept.
		{"no birth time, related unwire, file gone", false, false, "R2", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			real := dirIdentity
			reused := map[string]store.DirID{}
			dirIdentity = func(p string) (store.DirID, bool) {
				id, ok := real(p)
				if !ok {
					return id, ok
				}
				if r, isReused := reused[p]; isReused {
					id = r
				}
				if !tt.birth {
					id.Birth = 0
				} else if id.Birth == 0 {
					id.Birth = 1 // a file system with birth times
				}
				return id, true
			}
			t.Cleanup(func() { dirIdentity = real })

			dir := filepath.Join(f.dir, "made")
			c1 := apply(t, f, "R1", filepath.Join(dir, "x.toml"), "x\n")
			apply(t, f, "R2", filepath.Join(dir, "y.toml"), "y\n")
			apply(t, f, "R3", filepath.Join(f.dir, "other.toml"), "z\n")
			if _, err := f.m.Unwire(ctx, Selection{RunID: "R1"}, UnwireOptions{}, io.Discard); err != nil {
				t.Fatal(err)
			}
			// The person removes the directory and makes it again; it gets
			// the old inode number and a new birth time.
			must(t, os.RemoveAll(dir))
			must(t, os.Mkdir(dir, 0o755))
			got, _ := f.s.GetChange(ctx, c1.ChangeID)
			id := got.CreatedDirIDs[0]
			if tt.birth {
				id.Birth++
			}
			reused[dir] = id
			if tt.putBack {
				must(t, os.WriteFile(filepath.Join(dir, "y.toml"), []byte("y\n"), 0o600))
			}
			if _, err := f.m.Unwire(ctx, Selection{RunID: tt.run}, UnwireOptions{}, io.Discard); err != nil {
				t.Fatal(err)
			}
			_, err := os.Lstat(dir)
			if removed := err != nil; removed != tt.wantRemoved {
				t.Errorf("removed = %v (%v), want %v", removed, err, tt.wantRemoved)
			}
		})
	}
}
