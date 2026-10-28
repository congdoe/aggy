//go:build unix

package manifest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
)

func stateOf(t *testing.T, path string) string {
	t.Helper()
	st, err := patch.ReadState(path)
	must(t, err)
	return st.Hash
}

func applyP(t *testing.T, f fixture, run string, p patch.Patch) store.Change {
	t.Helper()
	c, err := f.m.Apply(context.Background(), run, p)
	must(t, err)
	if c.Status != store.ChangeApplied {
		t.Fatalf("%s: status %s", p.File, c.Status)
	}
	return c
}

// kindsFixture lays out a store directory and, under f.dir, a file, a
// link, and a path with nothing, then applies a link create, a link
// replacing the link, a link replacing the file, and a removal.
func kindsFixture(t *testing.T, f fixture) (paths []string) {
	t.Helper()
	storeDir := filepath.Join(f.dir, "store", "aggy")
	must(t, os.MkdirAll(storeDir, 0o755))
	file := filepath.Join(f.dir, "claude", "skills", "aggy")
	must(t, os.MkdirAll(filepath.Dir(file), 0o755))
	must(t, os.WriteFile(file, []byte("a copy\n"), 0o640))
	must(t, os.Chmod(file, 0o640))
	old := filepath.Join(f.dir, "agents", "skills", "aggy")
	must(t, os.MkdirAll(filepath.Dir(old), 0o755))
	must(t, os.Symlink("../../old/aggy", old))
	created := filepath.Join(f.dir, "grok", "skills", "aggy")
	gone := filepath.Join(f.dir, "stale.md")
	must(t, os.WriteFile(gone, []byte("stale\n"), 0o600))

	applyP(t, f, "r1", patch.Patch{File: created, ExpectedHash: patch.NoFile, Link: storeDir})
	applyP(t, f, "r1", patch.Patch{File: old, ExpectedHash: stateOf(t, old), Link: storeDir})
	applyP(t, f, "r1", patch.Patch{File: file, ExpectedHash: stateOf(t, file), Link: storeDir})
	applyP(t, f, "r1", patch.Patch{File: gone, ExpectedHash: stateOf(t, gone), Remove: true})
	return []string{created, old, file, gone}
}

// TestUnwireKindsGolden: the dry run of undoing every kind, then the
// undo itself leaves each path as it was, and the directories made for
// the new link are gone.
func TestUnwireKindsGolden(t *testing.T) {
	f := setup(t)
	paths := kindsFixture(t, f)
	before := map[string]string{
		paths[0]: patch.NoFile,
		paths[1]: patch.LinkHash("../../old/aggy"),
		paths[2]: patch.Hash([]byte("a copy\n")),
		paths[3]: patch.Hash([]byte("stale\n")),
	}

	var out bytes.Buffer
	if _, err := f.m.Unwire(context.Background(), Selection{All: true}, UnwireOptions{DryRun: true}, &out); err != nil {
		t.Fatal(err)
	}
	golden(t, "unwire_kinds.golden", strings.ReplaceAll(out.String(), f.dir, "$DIR"))
	for _, p := range paths[:3] {
		if stateOf(t, p) != patch.LinkHash(filepath.Join(f.dir, "store", "aggy")) {
			t.Errorf("the dry run changed %s", p)
		}
	}

	undos, err := f.m.Unwire(context.Background(), Selection{All: true}, UnwireOptions{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(undos) != 4 {
		t.Fatalf("undos = %+v", undos)
	}
	for _, p := range paths {
		if h := stateOf(t, p); h != before[p] {
			t.Errorf("%s: after unwire %s, want %s", p, h, before[p])
		}
	}
	if fi, err := os.Lstat(paths[2]); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("the replaced file's mode = %v, %v, want 0640", fi, err)
	}
	if fi, err := os.Lstat(paths[3]); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("the removed file's mode = %v, %v, want 0600", fi, err)
	}
	if _, err := os.Lstat(filepath.Join(f.dir, "grok")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the directories made for the new link remain: %v", err)
	}
}

// TestUnwireKindsRules: the unwire rules hold for link rows. A link
// retargeted after aggy made it is drift; a newer change to the same
// link blocks --change; two links to one target are separate paths.
func TestUnwireKindsRules(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	target := filepath.Join(f.dir, "store")
	must(t, os.MkdirAll(target, 0o755))
	a := filepath.Join(f.dir, "a")
	b := filepath.Join(f.dir, "b")
	ca := applyP(t, f, "r1", patch.Patch{File: a, ExpectedHash: patch.NoFile, Link: target})
	cb := applyP(t, f, "r2", patch.Patch{File: b, ExpectedHash: patch.NoFile, Link: target})

	// a is not blocked by b, although both point at one directory.
	if err := f.m.Revert(ctx, ca.ChangeID); err != nil {
		t.Fatalf("Revert a = %v", err)
	}
	if _, err := os.Lstat(a); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a is still there: %v", err)
	}
	if _, err := os.Lstat(target); err != nil {
		t.Errorf("the undo removed the link's target: %v", err)
	}

	// A newer change to b (retarget it) blocks undoing the older one.
	cb2 := applyP(t, f, "r3", patch.Patch{File: b, ExpectedHash: stateOf(t, b), Link: "elsewhere"})
	undos, err := f.m.Unwire(ctx, Selection{ChangeID: cb.ChangeID}, UnwireOptions{}, io.Discard)
	if !errors.Is(err, ErrNewerChange) || len(undos) != 1 || undos[0].Outcome != Blocked {
		t.Errorf("older link change = %+v, %v, want blocked", undos, err)
	}
	// Retargeted by hand after aggy's change: drift, left alone.
	must(t, os.Remove(b))
	must(t, os.Symlink("by-hand", b))
	undos, err = f.m.Unwire(ctx, Selection{ChangeID: cb2.ChangeID}, UnwireOptions{}, io.Discard)
	if !errors.Is(err, patch.ErrDrift) || len(undos) != 1 || undos[0].Outcome != Drifted {
		t.Errorf("retargeted link = %+v, %v, want drifted", undos, err)
	}
	if got, _ := os.Readlink(b); got != "by-hand" {
		t.Errorf("drifted link changed: %q", got)
	}
	// doctor agrees.
	ds, err := f.m.Drift(ctx)
	must(t, err)
	var drifted int
	for _, d := range ds {
		if d.Drifted {
			drifted++
		}
	}
	if drifted != 1 {
		t.Errorf("doctor drift = %+v", ds)
	}
}

// TestPreviewKindsGolden: setup's dry run shows each link and removal
// and writes nothing.
func TestPreviewKindsGolden(t *testing.T) {
	f := setup(t)
	file := filepath.Join(f.dir, "old.md")
	must(t, os.WriteFile(file, []byte("old\n"), 0o600))
	link := filepath.Join(f.dir, "new", "aggy")
	var out bytes.Buffer
	n, err := Preview(&out, []patch.Patch{
		{File: link, ExpectedHash: patch.NoFile, Link: "../store/aggy"},
		{File: file, ExpectedHash: patch.Hash([]byte("old\n")), Link: "../store/aggy"},
		{File: file, ExpectedHash: patch.Hash([]byte("old\n")), Remove: true},
	})
	if err != nil || n != 3 {
		t.Fatalf("Preview = %d, %v", n, err)
	}
	golden(t, "preview_kinds.golden", strings.ReplaceAll(out.String(), f.dir, "$DIR"))
	if _, err := os.Lstat(filepath.Dir(link)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("preview made a directory: %v", err)
	}
	if stateOf(t, file) != patch.Hash([]byte("old\n")) {
		t.Error("preview changed a file")
	}
}
