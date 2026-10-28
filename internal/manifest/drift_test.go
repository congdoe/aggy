package manifest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

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
			applyNew(t, f, "a.toml", "x\n")
		}, []want{{"a.toml", false, nil}}},
		{"edited after aggy wrote it", func(t *testing.T, f fixture) {
			applyNew(t, f, "a.toml", "x\n")
			writeDrift(t, filepath.Join(f.dir, "a.toml"), "edited\n")
		}, []want{{"a.toml", true, nil}}},
		{"removed after aggy wrote it", func(t *testing.T, f fixture) {
			applyNew(t, f, "a.toml", "x\n")
			if err := os.Remove(filepath.Join(f.dir, "a.toml")); err != nil {
				t.Fatal(err)
			}
		}, []want{{"a.toml", true, nil}}},
		{"two changes to one file: the newest is in effect", func(t *testing.T, f fixture) {
			applyNew(t, f, "a.toml", "x\n")
			p := filepath.Join(f.dir, "a.toml")
			cur, _ := patch.HashFile(p)
			if _, err := f.m.Apply(context.Background(), NewRunID(), patch.Patch{File: p, ExpectedHash: cur, Transform: func(b []byte) ([]byte, error) {
				if string(b) == "x\nmore\n" {
					return b, nil
				}
				return append(b, "more\n"...), nil
			}}); err != nil {
				t.Fatal(err)
			}
		}, []want{{"a.toml", false, nil}}},
		{"undone changes are not in effect", func(t *testing.T, f fixture) {
			c := applyNew(t, f, "a.toml", "x\n")
			if err := f.m.Revert(context.Background(), c.ChangeID); err != nil {
				t.Fatal(err)
			}
			writeDrift(t, filepath.Join(f.dir, "a.toml"), "edited\n")
		}, nil},
		{"pending: before, after, or neither", func(t *testing.T, f fixture) {
			before, after := patch.Hash([]byte("b\n")), patch.Hash([]byte("a\n"))
			for name, content := range map[string]string{"p1": "b\n", "p2": "a\n", "p3": "z\n"} {
				p := filepath.Join(f.dir, name)
				writeDrift(t, p, content)
				insertDrift(t, f, store.Change{ChangeID: "c-" + name, File: p, HashBefore: before, HashAfter: after, Status: store.ChangePending})
			}
		}, []want{{"p1", false, nil}, {"p2", false, nil}, {"p3", true, nil}}},
		{"a credential path is never read", func(t *testing.T, f fixture) {
			p := filepath.Join(os.Getenv("HOME"), ".codex", "auth.json")
			insertDrift(t, f, store.Change{ChangeID: "c-auth", File: p, HashBefore: patch.Hash(nil), HashAfter: patch.Hash([]byte("x")), Status: store.ChangeApplied})
		}, []want{{"auth.json", false, safefs.ErrAuthPath}}},
		{"a row aggy could not have written", func(t *testing.T, f fixture) {
			insertDrift(t, f, store.Change{ChangeID: "c-bad", File: "relative/x", HashBefore: patch.Hash(nil), HashAfter: "nothex", Status: store.ChangeApplied})
		}, []want{{"x", false, ErrInvalidRow}}},
		{"failed and noop rows are ignored", func(t *testing.T, f fixture) {
			p := filepath.Join(f.dir, "f")
			writeDrift(t, p, "z\n")
			insertDrift(t, f, store.Change{ChangeID: "c-f", File: p, HashBefore: patch.Hash(nil), HashAfter: patch.Hash([]byte("a")), Status: store.ChangeFailed})
			insertDrift(t, f, store.Change{ChangeID: "c-n", File: p, HashBefore: patch.Hash(nil), HashAfter: patch.Hash([]byte("a")), Status: store.ChangeNoop})
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t)
			tt.setup(t, f)
			got, err := f.m.Drift(context.Background())
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
		})
	}
}

func writeDrift(t *testing.T, p, content string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// applyNew creates name in f.dir holding content through the manifest.
func applyNew(t *testing.T, f fixture, name, content string) store.Change {
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

func insertDrift(t *testing.T, f fixture, c store.Change) {
	t.Helper()
	c.PlannedAt = time.Now()
	if err := f.s.InsertChange(context.Background(), c); err != nil {
		t.Fatal(err)
	}
}
