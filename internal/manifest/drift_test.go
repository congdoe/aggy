package manifest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// TestDrift checks Drift, which is an Unwire dry run, on the cases the
// review found doctor and unwire disagreeing on, and that it agrees with
// a locking dry run.
func TestDrift(t *testing.T) {
	type want struct {
		file    string // base name
		outcome Outcome
		err     error // matched with errors.Is; nil for none
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, f fixture) // makes files and changes in f.dir
		want  []want                        // oldest first
	}{
		{"no changes", func(*testing.T, fixture) {}, nil},
		{"unchanged", func(t *testing.T, f fixture) {
			createNew(t, f, "a.toml", "x\n")
		}, []want{{"a.toml", Removed, nil}}},
		{"edited after aggy wrote it", func(t *testing.T, f fixture) {
			createNew(t, f, "a.toml", "x\n")
			writeDrift(t, filepath.Join(f.dir, "a.toml"), "edited\n")
		}, []want{{"a.toml", Drifted, nil}}},
		// T6612 review 3: the next four are undone cleanly by unwire.
		{"created file removed by hand", func(t *testing.T, f fixture) {
			createNew(t, f, "a.toml", "x\n")
			if err := os.Remove(filepath.Join(f.dir, "a.toml")); err != nil {
				t.Fatal(err)
			}
		}, []want{{"a.toml", Removed, nil}}},
		{"modified file restored by hand", func(t *testing.T, f fixture) {
			changeExisting(t, f, "a.toml", "old\n", "old\nnew\n")
			writeDrift(t, filepath.Join(f.dir, "a.toml"), "old\n")
		}, []want{{"a.toml", Restored, nil}}},
		{"created file emptied", func(t *testing.T, f fixture) {
			createNew(t, f, "a.toml", "x\n")
			writeDrift(t, filepath.Join(f.dir, "a.toml"), " \n\n")
		}, []want{{"a.toml", Removed, nil}}},
		{"modified file undone but for a line ending", func(t *testing.T, f fixture) {
			changeExisting(t, f, "a.toml", "old", "old\nnew\n")
			writeDrift(t, filepath.Join(f.dir, "a.toml"), "old\r\n")
		}, []want{{"a.toml", Restored, nil}}},
		{"two changes to one file", func(t *testing.T, f fixture) {
			createNew(t, f, "a.toml", "x\n")
			changeExisting(t, f, "a.toml", "x\n", "x\nmore\n")
		}, []want{{"a.toml", Removed, nil}, {"a.toml", Restored, nil}}},
		{"undone changes are left out", func(t *testing.T, f fixture) {
			c := createNew(t, f, "a.toml", "x\n")
			if err := f.m.Revert(context.Background(), c.ChangeID); err != nil {
				t.Fatal(err)
			}
			writeDrift(t, filepath.Join(f.dir, "a.toml"), "edited\n")
		}, nil},
		// T6612 review 4: rows inserted in a fixed order.
		{"pending, never written", func(t *testing.T, f fixture) {
			for _, name := range []string{"p1", "p2"} {
				file := filepath.Join(f.dir, name)
				writeDrift(t, file, "b\n")
				insertDrift(t, f, store.Change{ChangeID: "c-" + name, File: file, HashBefore: patch.Hash([]byte("b\n")),
					HashAfter: patch.Hash([]byte("a\n")), Status: store.ChangePending})
			}
		}, []want{{"p1", Cleaned, nil}, {"p2", Cleaned, nil}}},
		{"a credential path is never read", func(t *testing.T, f fixture) {
			p := filepath.Join(os.Getenv("HOME"), ".codex", "auth.json")
			insertDrift(t, f, store.Change{ChangeID: "c-auth", File: p, HashBefore: patch.NoFile, HashAfter: patch.Hash([]byte("x")),
				CreatedFile: true, Status: store.ChangeApplied})
		}, []want{{"auth.json", Failed, safefs.ErrAuthPath}}},
		{"a row aggy could not have written", func(t *testing.T, f fixture) {
			insertDrift(t, f, store.Change{ChangeID: "c-bad", File: "relative/x", HashBefore: patch.Hash(nil), HashAfter: "nothex", Status: store.ChangeApplied})
		}, []want{{"x", Invalid, ErrInvalidRow}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t)
			tt.setup(t, f)
			ctx := context.Background()
			got, err := f.m.Drift(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("drift = %+v, want %+v", got, tt.want)
			}
			for i, w := range tt.want {
				g := got[i]
				if filepath.Base(g.Change.File) != w.file || g.Outcome != w.outcome || g.Drifted != (w.outcome == Drifted) ||
					(w.err == nil) != (g.Err == nil) || (w.err != nil && !errors.Is(g.Err, w.err)) {
					t.Errorf("%d: %s %s drifted %v err %v, want %+v", i, g.Change.File, g.Outcome, g.Drifted, g.Err, w)
				}
			}
			// The same answers as a dry run that takes its locks.
			undos, _ := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{DryRun: true}, io.Discard)
			if len(undos) != len(got) {
				t.Fatalf("unwire dry run: %d results, Drift %d", len(undos), len(got))
			}
			for i, u := range undos {
				if g := got[len(got)-1-i]; g.Change.ChangeID != u.Change.ChangeID || g.Outcome != u.Outcome {
					t.Errorf("%s: Drift says %s, unwire says %s", u.Change.File, g.Outcome, u.Outcome)
				}
			}
		})
	}
}

// TestDriftBlockedBy (T6612 re-review C, C1): an older change behind a
// drifted newer one is Blocked, and names the change actually in its
// way, not a newer one the dry run resolved.
func TestDriftBlockedBy(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, f fixture)
		// blocked and by are indexes into Drift's results, oldest first
		blocked, by int
	}{
		{"behind a drifted change", func(t *testing.T, f fixture) {
			createNew(t, f, "a.toml", "x\n")
			changeExisting(t, f, "a.toml", "x\n", "x\nmore\n")
			writeDrift(t, filepath.Join(f.dir, "a.toml"), "edited\n")
		}, 0, 1},
		{"a resolved failed change between them", func(t *testing.T, f fixture) {
			createNew(t, f, "a.toml", "x\n")
			insertDrift(t, f, store.Change{ChangeID: "c-failed", File: filepath.Join(f.dir, "a.toml"),
				HashBefore: patch.Hash([]byte("x\n")), HashAfter: patch.Hash([]byte("y\n")), Status: store.ChangeFailed})
			changeExisting(t, f, "a.toml", "x\n", "x\nmore\n")
			writeDrift(t, filepath.Join(f.dir, "a.toml"), "edited\n")
		}, 0, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t)
			tt.setup(t, f)
			got, err := f.m.Drift(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(got) <= tt.by || got[tt.blocked].Outcome != Blocked || got[tt.by].Outcome != Drifted ||
				got[tt.blocked].BlockedBy != got[tt.by].Change.ChangeID {
				t.Fatalf("drift = %+v", got)
			}
			// unwire's own error names the same change.
			if !strings.Contains(got[tt.blocked].Err.Error(), "change "+got[tt.by].Change.ChangeID+";") {
				t.Errorf("unwire blames another change: %v", got[tt.blocked].Err)
			}
		})
	}
}

// TestNoLockOnlyDryRun: NoLock is refused on a real unwire.
func TestNoLockOnlyDryRun(t *testing.T) {
	f := setup(t)
	if _, err := f.m.Unwire(context.Background(), Selection{All: true}, UnwireOptions{NoLock: true}, io.Discard); err == nil {
		t.Error("NoLock without DryRun accepted")
	}
}

// TestDriftWritesNothing: Drift leaves the state directory as it was:
// no lock files (an Unwire dry run with locks makes run/patch/ and a lock
// file per target), no backups.
func TestDriftWritesNothing(t *testing.T) {
	f := setup(t)
	createNew(t, f, "a.toml", "x\n")
	writeDrift(t, filepath.Join(f.dir, "a.toml"), "edited\n")
	if err := os.RemoveAll(filepath.Join(f.state, "run")); err != nil {
		t.Fatal(err)
	}
	before := listTree(t, f.state)
	if _, err := f.m.Drift(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := listTree(t, f.state); len(after) != len(before) {
		t.Errorf("Drift wrote to the state directory: %v -> %v", before, after)
	}
}

func listTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, _ os.DirEntry, err error) error {
		out = append(out, p)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func writeDrift(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// createNew creates name in f.dir holding content through the manifest.
func createNew(t *testing.T, f fixture, name, content string) store.Change {
	t.Helper()
	c, err := f.m.Apply(context.Background(), NewRunID(), patch.Patch{
		File: filepath.Join(f.dir, name), ExpectedHash: patch.NoFile,
		Transform: func([]byte) ([]byte, error) { return []byte(content), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// changeExisting makes name hold before (unless it already does) and
// changes it to after through the manifest.
func changeExisting(t *testing.T, f fixture, name, before, after string) {
	t.Helper()
	p := filepath.Join(f.dir, name)
	if cur, _ := os.ReadFile(p); string(cur) != before {
		writeDrift(t, p, before)
	}
	if _, err := f.m.Apply(context.Background(), NewRunID(), patch.Patch{
		File: p, ExpectedHash: patch.Hash([]byte(before)),
		Transform: func([]byte) ([]byte, error) { return []byte(after), nil },
	}); err != nil {
		t.Fatal(err)
	}
}

func insertDrift(t *testing.T, f fixture, c store.Change) {
	t.Helper()
	c.PlannedAt = time.Now()
	if err := f.s.InsertChange(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}
