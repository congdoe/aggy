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

// Regression tests for the PR #7 review. Each fails on 2694c90.

// setText applies a patch that sets file to text.
func setText(t *testing.T, f fixture, run, file, text string) store.Change {
	t.Helper()
	h, err := patch.HashFile(file)
	must(t, err)
	c, err := f.m.Apply(context.Background(), run, patch.Patch{File: file, ExpectedHash: h,
		Transform: func([]byte) ([]byte, error) { return []byte(text), nil }})
	must(t, err)
	return c
}

// TestSupersedeProofMustBeInEffect (finding 1): a newer change planned
// on C's hash before proves C unwritten only while it is still in
// effect. Once it is undone, the file may hold C's write again.
func TestSupersedeProofMustBeInEffect(t *testing.T) {
	for _, pDrifts := range []bool{false, true} {
		name := "run C then all"
		if pDrifts {
			name = "P drifted, all"
		}
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			file := filepath.Join(f.dir, "c.toml")
			must(t, os.WriteFile(file, []byte("X\n"), 0o600))
			c := pendingWritten(t, f, "C", file, "A\n") // X+A, C pending
			m := setText(t, f, "M", file, "X\n")        // A -> X
			n := apply(t, f, "N", file, "N\n")          // planned on X: C's hash before
			for _, id := range []string{n.ChangeID, m.ChangeID} {
				if _, err := f.m.Unwire(ctx, Selection{ChangeID: id}, UnwireOptions{}, io.Discard); err != nil {
					t.Fatal(err)
				}
			}
			if b, _ := os.ReadFile(file); string(b) != "X\nA\n" {
				t.Fatalf("after undoing N and M = %q", b)
			}
			must(t, os.WriteFile(file, []byte("X\nA\nuser\n"), 0o600))
			apply(t, f, "P", file, "P\n")
			if pDrifts {
				must(t, os.WriteFile(file, []byte("X\nA\nuser\nP\nmore\n"), 0o600))
			} else {
				undos, _ := f.m.Unwire(ctx, Selection{RunID: "C"}, UnwireOptions{}, io.Discard)
				if got := outcomeOf(undos, c.ChangeID); got != Blocked {
					t.Errorf("--run C: C = %q, want blocked", got)
				}
			}
			undos, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard)
			if err == nil {
				t.Error("--all reported no error with C's line still in the file")
			}
			if got := outcomeOf(undos, c.ChangeID); got == Superseded || got == "" {
				t.Errorf("--all: C = %q, want it reported", got)
			}
			if s := status(t, f, c.ChangeID); s != store.ChangePending {
				t.Errorf("C = %s, want still pending", s)
			}
		})
	}
}

// TestSweepAfterCrashMidUndo (finding 2): R2's undo removed its file and
// stopped before marking R2 undone (or the person deleted the file). The
// next --all must still sweep R1's directory, with or without birth
// times.
func TestSweepAfterCrashMidUndo(t *testing.T) {
	for _, birth := range []bool{false, true} {
		t.Run(map[bool]string{false: "no birth time", true: "birth time"}[birth], func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			real := dirIdentity
			dirIdentity = func(p string) (store.DirID, bool) {
				id, ok := real(p)
				if ok && !birth {
					id.Birth = 0
				} else if ok && id.Birth == 0 {
					id.Birth = 1
				}
				return id, ok
			}
			t.Cleanup(func() { dirIdentity = real })
			dir := filepath.Join(f.dir, "made")
			apply(t, f, "R1", filepath.Join(dir, "x.toml"), "x\n")
			apply(t, f, "R2", filepath.Join(dir, "y.toml"), "y\n")
			if _, err := f.m.Unwire(ctx, Selection{RunID: "R1"}, UnwireOptions{}, io.Discard); err != nil {
				t.Fatal(err)
			}
			must(t, os.Remove(filepath.Join(dir, "y.toml")))
			if _, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(dir); err == nil {
				t.Error("aggy's directory left behind")
			}
		})
	}
}

// TestUnwrittenRowsAreCleaned (finding 3): a failed row never wrote, and
// a pending row of an existing file with no backup never wrote either
// (the backup is synced before the target write). Neither is drift, so
// neither is stuck: the file is left as it is and the row is undone.
func TestUnwrittenRowsAreCleaned(t *testing.T) {
	tests := []struct {
		name    string
		pending bool // set the failed row back to pending
		noBak   bool // remove its backup
		want    Outcome
	}{
		{"failed, backup kept", false, false, Cleaned},
		{"failed, no backup", false, true, Cleaned},
		{"pending, no backup", true, true, Cleaned},
		{"pending, backup kept", true, false, Drifted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			file := filepath.Join(f.dir, "c.toml")
			must(t, os.WriteFile(file, []byte("X\n"), 0o600))
			restore := patch.SetTestHookBeforeWrite(func(string) error { return errors.New("boom") })
			_, err := f.m.Apply(ctx, "A", patch.Patch{File: file, ExpectedHash: patch.Hash([]byte("X\n")), Transform: appendText("A\n")})
			restore()
			if err == nil {
				t.Fatal("A did not fail")
			}
			cs, _ := f.s.Changes(ctx, "A")
			a := cs[0]
			if tt.pending {
				if _, err := f.s.DB().ExecContext(ctx, `UPDATE changes SET status = 'pending' WHERE change_id = ?`, a.ChangeID); err != nil {
					t.Fatal(err)
				}
			}
			if tt.noBak {
				must(t, os.RemoveAll(filepath.Join(f.state, BackupDirName, a.ChangeID)))
			}
			must(t, os.WriteFile(file, []byte("X\nmine\n"), 0o600))
			undos, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard)
			if got := outcomeOf(undos, a.ChangeID); got != tt.want {
				t.Fatalf("A = %q (%v), want %q", got, err, tt.want)
			}
			if b, _ := os.ReadFile(file); string(b) != "X\nmine\n" {
				t.Errorf("file = %q, want the person's edit kept", b)
			}
			if tt.want == Cleaned {
				if err != nil {
					t.Errorf("Unwire = %v", err)
				}
				if s := status(t, f, a.ChangeID); s != store.ChangeUndone {
					t.Errorf("A = %s, want undone", s)
				}
			}
		})
	}
}

// TestUnwrittenFailedUnderNewer (finding 3): a failed row is cleaned
// even with a newer change in place, and the newer change is untouched.
func TestUnwrittenFailedUnderNewer(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "c.toml")
	must(t, os.WriteFile(file, []byte("X\n"), 0o600))
	restore := patch.SetTestHookBeforeWrite(func(string) error { return errors.New("boom") })
	_, _ = f.m.Apply(ctx, "A", patch.Patch{File: file, ExpectedHash: patch.Hash([]byte("X\n")), Transform: appendText("A\n")})
	restore()
	must(t, os.WriteFile(file, []byte("X\nuser\n"), 0o600))
	b := apply(t, f, "B", file, "B\n")
	undos, err := f.m.Unwire(ctx, Selection{RunID: "A"}, UnwireOptions{}, io.Discard)
	cs, _ := f.s.Changes(ctx, "A")
	if got := outcomeOf(undos, cs[0].ChangeID); got != Cleaned || err != nil {
		t.Errorf("A = %q, %v, want cleaned", got, err)
	}
	if s := status(t, f, b.ChangeID); s != store.ChangeApplied {
		t.Errorf("B = %s", s)
	}
	if got, _ := os.ReadFile(file); string(got) != "X\nuser\nB\n" {
		t.Errorf("file = %q", got)
	}
}
