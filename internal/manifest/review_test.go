package manifest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// Regression tests for the PR #4 review (C1, C2, C3, C7, C12). Each fails
// on 522a431, with the test hooks it uses added there.

// TestBackupSyncFailureStillApplies (C1): a failed sync of the backup's
// directory is not a failed change, and never reads as a no-op.
func TestBackupSyncFailureStillApplies(t *testing.T) {
	f := setup(t)
	file := filepath.Join(f.dir, "config.toml")
	orig := "a = 1\n"
	must(t, os.WriteFile(file, []byte(orig), 0o600))
	backups := filepath.Join(f.state, BackupDirName)
	t.Cleanup(safefs.SetTestDirSync(func(dir string) error {
		if strings.HasPrefix(dir, backups) || strings.Contains(dir, BackupDirName) {
			return errors.New("EIO")
		}
		return nil
	}))
	c, err := f.m.Apply(context.Background(), "r", patch.Patch{File: file, ExpectedHash: patch.Hash([]byte(orig)), Transform: addLine})
	if err != nil {
		t.Fatalf("Apply = %v, want success", err)
	}
	if c.Status != store.ChangeApplied || c.BackupPath == "" {
		t.Errorf("change = %+v, want applied with a backup", c)
	}
	if b, _ := os.ReadFile(file); string(b) != orig+line {
		t.Errorf("file = %q, the change was not made", b)
	}
}

// TestTargetSyncFailureWarns (C1): a failed sync after the target write
// is recorded as applied and says which write it was.
func TestTargetSyncFailureWarns(t *testing.T) {
	f := setup(t)
	file := filepath.Join(f.dir, "config.toml")
	orig := "a = 1\n"
	must(t, os.WriteFile(file, []byte(orig), 0o600))
	t.Cleanup(safefs.SetTestDirSync(func(dir string) error {
		if !strings.Contains(dir, BackupDirName) {
			return errors.New("EIO")
		}
		return nil
	}))
	c, err := f.m.Apply(context.Background(), "r", patch.Patch{File: file, ExpectedHash: patch.Hash([]byte(orig)), Transform: addLine})
	if !errors.Is(err, patch.ErrNotSynced) || !strings.Contains(err.Error(), "target") {
		t.Fatalf("Apply = %v, want a target ErrNotSynced", err)
	}
	if c.Status != store.ChangeApplied {
		t.Errorf("status = %s, want applied", c.Status)
	}
}

// TestConcurrentApplySameFile (C2): two changes to one file planned
// against the same content cannot both land.
func TestConcurrentApplySameFile(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "config.toml")
	orig := "x = 0\n"
	must(t, os.WriteFile(file, []byte(orig), 0o600))
	// Hold each writer at the write until both are there, or 500ms.
	var arrived atomic.Int32
	t.Cleanup(patch.SetTestHookBeforeWrite(func(string) error {
		arrived.Add(1)
		deadline := time.Now().Add(500 * time.Millisecond)
		for arrived.Load() < 2 && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		return nil
	}))
	appendText := func(text string) func([]byte) ([]byte, error) {
		return func(b []byte) ([]byte, error) {
			if bytes.Contains(b, []byte(text)) {
				return b, nil
			}
			return append(append([]byte{}, b...), text...), nil
		}
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, text := range []string{"a = 1\n", "b = 2\n"} {
		wg.Go(func() {
			m := New(f.s, filepath.Join(f.state, BackupDirName))
			_, errs[i] = m.Apply(ctx, "r", patch.Patch{File: file, ExpectedHash: patch.Hash([]byte(orig)), Transform: appendText(text)})
		})
	}
	wg.Wait()
	drift := 0
	for _, err := range errs {
		if errors.Is(err, patch.ErrDrift) {
			drift++
		} else if err != nil {
			t.Errorf("Apply = %v", err)
		}
	}
	if drift != 1 {
		t.Errorf("%d of 2 refused as drift, want exactly 1 (errors %v)", drift, errs)
	}
	cs, _ := f.s.Changes(ctx, "r")
	applied := 0
	for _, c := range cs {
		if c.Status == store.ChangeApplied {
			applied++
			b, _ := os.ReadFile(file)
			if patch.Hash(b) != c.HashAfter {
				t.Error("the applied change is not what the file holds: an update was lost")
			}
		}
	}
	if applied != 1 {
		t.Errorf("%d applied rows, want 1", applied)
	}
}

// TestFailedCreateKeepsDirs (C3): the directories a new file needs are in
// the pending row, and stay on a failed one.
func TestFailedCreateKeepsDirs(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "a", "b", "settings.json")
	t.Cleanup(patch.SetTestHookBeforeWrite(func(string) error { return errors.New("crash") }))
	if _, err := f.m.Apply(ctx, "r", patch.Patch{File: file, ExpectedHash: patch.NoFile, Transform: addLine}); err == nil {
		t.Fatal("Apply succeeded")
	}
	cs, _ := f.s.Changes(ctx, "r")
	want := []string{filepath.Join(f.dir, "a"), filepath.Join(f.dir, "a", "b")}
	if len(cs) != 1 || cs[0].Status != store.ChangeFailed || len(cs[0].CreatedDirs) != 2 ||
		cs[0].CreatedDirs[0] != want[0] || cs[0].CreatedDirs[1] != want[1] {
		t.Errorf("changes = %+v, want one failed row naming %v", cs, want)
	}
}

// TestRevertFinishesHalfDoneCreate (C7): a revert that removed the file
// but not its directories finishes on the retry.
func TestRevertFinishesHalfDoneCreate(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "a", "b", "settings.json")
	c, err := f.m.Apply(ctx, "r", patch.Patch{File: file, ExpectedHash: patch.NoFile, Transform: addLine})
	if err != nil {
		t.Fatal(err)
	}
	must(t, os.Remove(file)) // the first revert stopped here
	if err := f.m.Revert(ctx, c.ChangeID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "a")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("created directories left: %v", err)
	}
	cs, err := f.s.ChangesNewestFirst(ctx, "")
	if err != nil || len(cs) != 1 {
		t.Errorf("ChangesNewestFirst = %v, %v", cs, err)
	}
}

// TestSetupModeWithoutStore (C12): with no index, setup is a first setup,
// and asking does not create one.
func TestSetupModeWithoutStore(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	state := t.TempDir()
	t.Setenv(home.Env, state)
	mode, err := SetupMode(context.Background(), false, false)
	if err != nil || mode != DryRun {
		t.Fatalf("SetupMode = %v, %v, want dry-run", mode, err)
	}
	if entries, _ := os.ReadDir(state); len(entries) != 0 {
		t.Errorf("SetupMode created %d files", len(entries))
	}
}

// TestSetupModeAfterNoopWrite (C12): a write run that finds nothing to
// change ends the first-setup dry runs.
func TestSetupModeAfterNoopWrite(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "done.toml")
	must(t, os.WriteFile(file, []byte(line), 0o600))
	ps := []patch.Patch{{File: file, ExpectedHash: patch.Hash([]byte(line)), Transform: addLine}}
	if _, err := f.m.Run(ctx, Write, NewRunID(), ps, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if mode, err := SetupMode(ctx, false, false); err != nil || mode != Write {
		t.Errorf("after a no-op write, SetupMode = %v, %v, want write", mode, err)
	}
}
