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
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// TestDrift checks Drift against hand-written answers and, for every
// checkable target, against what an Unwire dry run says of the same
// change: doctor must never call drifted what unwire undoes without
// --force, or the reverse.
func TestDrift(t *testing.T) {
	type want struct {
		file    string // base name
		drifted bool
		err     error // matched with errors.Is; nil for none
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, f fixture) // makes files and changes in f.dir
		want  []want
	}{
		{"no changes", func(*testing.T, fixture) {}, nil},
		{"unchanged", func(t *testing.T, f fixture) {
			createNew(t, f, "a.toml", "x\n")
		}, []want{{"a.toml", false, nil}}},
		{"edited after aggy wrote it", func(t *testing.T, f fixture) {
			createNew(t, f, "a.toml", "x\n")
			writeDrift(t, filepath.Join(f.dir, "a.toml"), "edited\n")
		}, []want{{"a.toml", true, nil}}},
		// T6612 review 3: the next four are undone cleanly by unwire.
		{"created file removed by hand", func(t *testing.T, f fixture) {
			createNew(t, f, "a.toml", "x\n")
			if err := os.Remove(filepath.Join(f.dir, "a.toml")); err != nil {
				t.Fatal(err)
			}
		}, []want{{"a.toml", false, nil}}},
		{"modified file restored by hand", func(t *testing.T, f fixture) {
			changeExisting(t, f, "a.toml", "old\n", "old\nnew\n")
			writeDrift(t, filepath.Join(f.dir, "a.toml"), "old\n")
		}, []want{{"a.toml", false, nil}}},
		{"created file emptied", func(t *testing.T, f fixture) {
			createNew(t, f, "a.toml", "x\n")
			writeDrift(t, filepath.Join(f.dir, "a.toml"), " \n\n")
		}, []want{{"a.toml", false, nil}}},
		{"modified file undone but for a line ending", func(t *testing.T, f fixture) {
			changeExisting(t, f, "a.toml", "old", "old\nnew\n")
			writeDrift(t, filepath.Join(f.dir, "a.toml"), "old\r\n")
		}, []want{{"a.toml", false, nil}}},
		{"two changes to one file: the newest is in effect", func(t *testing.T, f fixture) {
			createNew(t, f, "a.toml", "x\n")
			changeExisting(t, f, "a.toml", "x\n", "x\nmore\n")
		}, []want{{"a.toml", false, nil}}},
		{"undone changes are not in effect", func(t *testing.T, f fixture) {
			c := createNew(t, f, "a.toml", "x\n")
			if err := f.m.Revert(context.Background(), c.ChangeID); err != nil {
				t.Fatal(err)
			}
			writeDrift(t, filepath.Join(f.dir, "a.toml"), "edited\n")
		}, nil},
		// T6612 review 4: rows inserted in a fixed order.
		{"pending: before, after, or neither", func(t *testing.T, f fixture) {
			before, after := patch.Hash([]byte("b\n")), patch.Hash([]byte("a\n"))
			for _, p := range []struct{ name, content string }{{"p1", "b\n"}, {"p2", "a\n"}, {"p3", "z\n"}} {
				file := filepath.Join(f.dir, p.name)
				writeDrift(t, file, p.content)
				insertDrift(t, f, store.Change{ChangeID: "c-" + p.name, File: file, HashBefore: before, HashAfter: after, Status: store.ChangePending})
			}
		}, []want{{"p1", false, nil}, {"p2", false, nil}, {"p3", true, nil}}},
		{"a failed row is checked as unwire checks it", func(t *testing.T, f fixture) {
			p := filepath.Join(f.dir, "f")
			writeDrift(t, p, "z\n")
			insertDrift(t, f, store.Change{ChangeID: "c-f", File: p, HashBefore: patch.Hash([]byte("y\n")), HashAfter: patch.Hash([]byte("a")), Status: store.ChangeFailed})
			insertDrift(t, f, store.Change{ChangeID: "c-n", File: filepath.Join(f.dir, "n"), HashBefore: patch.Hash(nil), HashAfter: patch.Hash([]byte("a")), Status: store.ChangeNoop})
		}, []want{{"f", true, nil}}},
		{"a credential path is never read", func(t *testing.T, f fixture) {
			p := filepath.Join(os.Getenv("HOME"), ".codex", "auth.json")
			insertDrift(t, f, store.Change{ChangeID: "c-auth", File: p, HashBefore: patch.Hash(nil), HashAfter: patch.Hash([]byte("x")), Status: store.ChangeApplied})
		}, []want{{"auth.json", false, safefs.ErrAuthPath}}},
		{"a row aggy could not have written", func(t *testing.T, f fixture) {
			insertDrift(t, f, store.Change{ChangeID: "c-bad", File: "relative/x", HashBefore: patch.Hash(nil), HashAfter: "nothex", Status: store.ChangeApplied})
		}, []want{{"x", false, ErrInvalidRow}}},
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
				if filepath.Base(g.Change.File) != w.file || g.Drifted != w.drifted || (w.err == nil) != (g.Err == nil) || (w.err != nil && !errors.Is(g.Err, w.err)) {
					t.Errorf("%d: %s drifted %v err %v, want %+v", i, g.Change.File, g.Drifted, g.Err, w)
				}
			}
			// The dry run here may take target locks; the test's state
			// directory is a temp dir.
			undos, _ := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{DryRun: true}, io.Discard)
			outcome := map[string]Outcome{}
			for _, u := range undos {
				outcome[u.Change.ChangeID] = u.Outcome
			}
			for _, g := range got {
				if g.Err != nil {
					continue
				}
				if o := outcome[g.Change.ChangeID]; (o == Drifted) != g.Drifted {
					t.Errorf("%s: Drift says drifted %v, unwire says %s", g.Change.File, g.Drifted, o)
				}
			}
		})
	}
}

// TestDriftWritesNothing: Drift leaves the state directory as it was (no
// lock files, no backups).
func TestDriftWritesNothing(t *testing.T) {
	f := setup(t)
	createNew(t, f, "a.toml", "x\n")
	writeDrift(t, filepath.Join(f.dir, "a.toml"), "edited\n")
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
