package manifest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
)

const line = "project_doc_fallback_filenames = [\"CLAUDE.md\"]\n"

func addLine(b []byte) ([]byte, error) {
	if strings.Contains(string(b), line) {
		return b, nil
	}
	return append(append([]byte{}, b...), line...), nil
}

type fixture struct {
	m     *Manifest
	s     *store.Store
	state string
	dir   string
}

func setup(t *testing.T) fixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	state := t.TempDir()
	t.Setenv(home.Env, state)
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	m, err := Open(s)
	if err != nil {
		t.Fatal(err)
	}
	return fixture{m: m, s: s, state: state, dir: t.TempDir()}
}

func TestApplyRecordsAndReverts(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "config.toml")
	orig := "model = \"o\"\n"
	if err := os.WriteFile(file, []byte(orig), 0o640); err != nil {
		t.Fatal(err)
	}
	run := NewRunID()
	c, err := f.m.Apply(ctx, run, patch.Patch{File: file, ExpectedHash: patch.Hash([]byte(orig)), Transform: addLine})
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != store.ChangeApplied || c.RunID != run || c.HashBefore != patch.Hash([]byte(orig)) ||
		c.HashAfter != patch.Hash([]byte(orig+line)) || c.FileMode != 0o640 || c.CreatedFile || c.AppliedAt.IsZero() {
		t.Errorf("change = %+v", c)
	}
	// The backup sits in backups/<change-id> under AGGY_HOME.
	wantDir := filepath.Join(f.state, BackupDirName, c.ChangeID)
	if filepath.Dir(c.BackupPath) != wantDir {
		t.Errorf("backup = %s, want it in %s", c.BackupPath, wantDir)
	}
	if b, err := os.ReadFile(c.BackupPath); err != nil || string(b) != orig {
		t.Errorf("backup content = %q, %v", b, err)
	}

	// A second apply is a no-op and records nothing.
	c2, err := f.m.Apply(ctx, run, patch.Patch{File: file, ExpectedHash: patch.Hash([]byte(orig)), Transform: addLine})
	if err != nil || c2.Status != store.ChangeNoop || c2.ChangeID != "" {
		t.Errorf("reapply = %+v, %v", c2, err)
	}
	if cs, _ := f.s.Changes(ctx, run); len(cs) != 1 {
		t.Errorf("changes = %d, want 1", len(cs))
	}

	if err := f.m.Revert(ctx, c.ChangeID); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(file); string(b) != orig {
		t.Errorf("after revert = %q", b)
	}
	got, err := f.s.GetChange(ctx, c.ChangeID)
	if err != nil || got.Status != store.ChangeUndone || got.UndoneAt.IsZero() {
		t.Errorf("after revert = %+v, %v", got, err)
	}
	if err := f.m.Revert(ctx, c.ChangeID); err == nil {
		t.Error("second revert succeeded")
	}
}

func TestApplyCreateRecordsDirs(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "a", "b", "settings.json")
	c, err := f.m.Apply(ctx, "r", patch.Patch{File: file, ExpectedHash: patch.NoFile, Transform: addLine, Mode: 0o644})
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.s.GetChange(ctx, c.ChangeID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(f.dir, "a"), filepath.Join(f.dir, "a", "b")}
	if !got.CreatedFile || got.FileMode != 0o644 || got.BackupPath != "" || len(got.CreatedDirs) != 2 ||
		got.CreatedDirs[0] != want[0] || got.CreatedDirs[1] != want[1] || got.HashBefore != patch.NoFile {
		t.Errorf("change = %+v", got)
	}
	if err := f.m.Revert(ctx, c.ChangeID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("created dirs left: %v", err)
	}
}

func TestRefusedPatchRecordsNothing(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "config.toml")
	if err := os.WriteFile(file, []byte("changed by the user\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := f.m.Apply(ctx, "r", patch.Patch{File: file, ExpectedHash: patch.Hash([]byte("old")), Transform: addLine})
	if !errors.Is(err, patch.ErrDrift) {
		t.Fatalf("Apply = %v, want drift", err)
	}
	if cs, _ := f.s.Changes(ctx, ""); len(cs) != 0 {
		t.Errorf("refused patch recorded %d changes", len(cs))
	}
	if _, err := os.Stat(filepath.Join(f.state, BackupDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("refused patch left a backup folder: %v", err)
	}
}

func TestFailedWriteMarkedFailed(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "config.toml")
	if err := os.WriteFile(file, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A file where the backups folder should be makes the backup fail
	// after the pending row is written.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	m := New(f.s, blocker)
	if _, err := m.Apply(ctx, "r", patch.Patch{File: file, ExpectedHash: patch.Hash([]byte("x\n")), Transform: addLine}); err == nil {
		t.Fatal("Apply succeeded with no backup folder")
	}
	cs, _ := f.s.Changes(ctx, "r")
	if len(cs) != 1 || cs[0].Status != store.ChangeFailed {
		t.Errorf("changes = %+v, want one failed", cs)
	}
	if b, _ := os.ReadFile(file); string(b) != "x\n" {
		t.Errorf("file changed: %q", b)
	}
}
