package manifest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
)

// appendText returns an idempotent transform that appends text once.
func appendText(text string) func([]byte) ([]byte, error) {
	return func(b []byte) ([]byte, error) {
		if bytes.Contains(b, []byte(text)) {
			return b, nil
		}
		return append(append([]byte{}, b...), text...), nil
	}
}

func apply(t *testing.T, f fixture, run, file, text string) store.Change {
	t.Helper()
	h, err := patch.HashFile(file)
	must(t, err)
	c, err := f.m.Apply(context.Background(), run, patch.Patch{File: file, ExpectedHash: h, Transform: appendText(text)})
	must(t, err)
	return c
}

func status(t *testing.T, f fixture, id string) string {
	t.Helper()
	c, err := f.s.GetChange(context.Background(), id)
	must(t, err)
	return c.Status
}

func TestUnwireRestoresNewestFirst(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "config.toml")
	orig := "# keep me\r\nmodel = \"o\""
	must(t, os.WriteFile(file, []byte(orig), 0o640))
	created := filepath.Join(f.dir, "new", "deeper", "settings.json")
	c1 := apply(t, f, "r1", file, "\na = 1\n")
	c2 := apply(t, f, "r2", file, "b = 2\n")
	c3 := apply(t, f, "r2", created, "{}\n")
	// The person changes only the mode; the content is aggy's.
	must(t, os.Chmod(file, 0o604))

	undos, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, u := range undos {
		order = append(order, u.Change.ChangeID)
	}
	if strings.Join(order, ",") != strings.Join([]string{c3.ChangeID, c2.ChangeID, c1.ChangeID}, ",") {
		t.Errorf("undo order = %v, want newest first", order)
	}
	if b, _ := os.ReadFile(file); string(b) != orig {
		t.Errorf("file = %q, want %q", b, orig)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", fi.Mode().Perm())
	}
	if _, err := os.Lstat(filepath.Join(f.dir, "new")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("created dirs left behind: %v", err)
	}
	for _, c := range []store.Change{c1, c2, c3} {
		if s := status(t, f, c.ChangeID); s != store.ChangeUndone {
			t.Errorf("%s status = %s, want undone", c.ChangeID, s)
		}
	}
	// A second unwire has nothing to do.
	if undos, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard); err != nil || len(undos) != 0 {
		t.Errorf("second unwire = %+v, %v", undos, err)
	}
}

func TestUnwireRefusesDrift(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	a := filepath.Join(f.dir, "a.toml")
	b := filepath.Join(f.dir, "b.toml")
	must(t, os.WriteFile(a, []byte("a\n"), 0o600))
	must(t, os.WriteFile(b, []byte("b\n"), 0o600))
	ca := apply(t, f, "r", a, "x\n")
	cb := apply(t, f, "r", b, "y\n")
	edited := "a\nx\nmine\n"
	must(t, os.WriteFile(a, []byte(edited), 0o600))

	undos, err := f.m.Unwire(ctx, Selection{RunID: "r"}, UnwireOptions{}, io.Discard)
	if !errors.Is(err, patch.ErrDrift) {
		t.Errorf("Unwire = %v, want ErrDrift", err)
	}
	got := map[string]Outcome{}
	for _, u := range undos {
		got[u.Change.ChangeID] = u.Outcome
	}
	if got[ca.ChangeID] != Drifted || got[cb.ChangeID] != Restored {
		t.Errorf("outcomes = %v", got)
	}
	if bs, _ := os.ReadFile(a); string(bs) != edited {
		t.Errorf("drifted file touched: %q", bs)
	}
	if bs, _ := os.ReadFile(b); string(bs) != "b\n" {
		t.Errorf("other file not restored: %q", bs)
	}
	if s := status(t, f, ca.ChangeID); s != store.ChangeApplied {
		t.Errorf("drifted change status = %s, want applied", s)
	}

	// Forced, the edit is saved first and the file restored.
	undos, err = f.m.Unwire(ctx, Selection{ChangeID: ca.ChangeID}, UnwireOptions{Force: true}, io.Discard)
	if err != nil || len(undos) != 1 || undos[0].Outcome != Restored || undos[0].Saved == "" {
		t.Fatalf("forced = %+v, %v", undos, err)
	}
	if bs, _ := os.ReadFile(a); string(bs) != "a\n" {
		t.Errorf("forced file = %q", bs)
	}
	if bs, _ := os.ReadFile(undos[0].Saved); string(bs) != edited {
		t.Errorf("saved edit = %q", bs)
	}
}

func TestUnwireDryRunWritesNothing(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "c.toml")
	must(t, os.WriteFile(file, []byte("k = 1\n"), 0o600))
	created := filepath.Join(f.dir, "n", "x.toml")
	c1 := apply(t, f, "r", file, "\x1b[2J = 2\n")
	apply(t, f, "r", created, "z = 3\n")
	var out bytes.Buffer
	undos, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{DryRun: true}, &out)
	if err != nil || len(undos) != 2 {
		t.Fatalf("dry run = %+v, %v", undos, err)
	}
	got := out.String()
	for _, want := range []string{"--- " + created + "\n+++ /dev/null", "-z = 3", "--- " + file, `-\x1b[2J = 2`, filepath.Join(f.dir, "n") + ": would remove the directory if empty"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry run lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("dry run holds a raw escape: %q", got)
	}
	if bs, _ := os.ReadFile(file); !strings.Contains(string(bs), "= 2") {
		t.Error("dry run changed the file")
	}
	if s := status(t, f, c1.ChangeID); s != store.ChangeApplied {
		t.Errorf("dry run marked %s", s)
	}
}

// TestUnwirePendingWritten: a pending row whose write happened (the
// process stopped before recording it) is undone from its backup.
func TestUnwirePendingWritten(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "c.toml")
	must(t, os.WriteFile(file, []byte("k = 1\n"), 0o600))
	c := apply(t, f, "r", file, "w = 2\n")
	// What a crash between the write and FinishChange leaves.
	if _, err := f.s.DB().ExecContext(ctx, `UPDATE changes SET status = 'pending', backup_path = '' WHERE change_id = ?`, c.ChangeID); err != nil {
		t.Fatal(err)
	}
	undos, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard)
	if err != nil || len(undos) != 1 || undos[0].Outcome != Restored {
		t.Fatalf("Unwire = %+v, %v", undos, err)
	}
	if bs, _ := os.ReadFile(file); string(bs) != "k = 1\n" {
		t.Errorf("file = %q", bs)
	}
	if s := status(t, f, c.ChangeID); s != store.ChangeUndone {
		t.Errorf("status = %s", s)
	}
}

// TestUnwireFailedCreateCleansDirs: a failed create that made its
// directories but never wrote the file leaves nothing behind.
func TestUnwireFailedCreateCleansDirs(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "made", "for", "it.toml")
	restore := patch.SetTestHookBeforeWrite(func(string) error { return errors.New("boom") })
	_, err := f.m.Apply(ctx, "r", patch.Patch{File: file, ExpectedHash: patch.NoFile, Transform: appendText("x\n")})
	restore()
	if err == nil {
		t.Fatal("Apply succeeded")
	}
	if _, err := os.Lstat(filepath.Dir(file)); err != nil {
		t.Fatalf("setup: dirs not made: %v", err)
	}
	undos, err := f.m.Unwire(ctx, Selection{RunID: "r"}, UnwireOptions{}, io.Discard)
	if err != nil || len(undos) != 1 || undos[0].Outcome != Cleaned {
		t.Fatalf("Unwire = %+v, %v", undos, err)
	}
	if _, err := os.Lstat(filepath.Join(f.dir, "made")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dirs left behind: %v", err)
	}
	if s := status(t, f, undos[0].Change.ChangeID); s != store.ChangeUndone {
		t.Errorf("status = %s", s)
	}
}

// TestUnwireSkipsMarkersAndNoops: the all-noop marker row of a run that
// changed nothing is not an undo.
func TestUnwireSkipsMarkersAndNoops(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "c.toml")
	must(t, os.WriteFile(file, []byte(line), 0o600))
	cs, err := f.m.Run(ctx, Write, "r", []patch.Patch{{File: file, ExpectedHash: patch.Hash([]byte(line)), Transform: addLine}}, io.Discard)
	if err != nil || len(cs) != 1 || cs[0].Status != store.ChangeNoop {
		t.Fatalf("Run = %+v, %v", cs, err)
	}
	undos, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{}, io.Discard)
	if err != nil || len(undos) != 0 {
		t.Errorf("Unwire = %+v, %v, want nothing", undos, err)
	}
	if _, err := f.m.Unwire(ctx, Selection{}, UnwireOptions{}, io.Discard); err == nil {
		t.Error("an empty selection was accepted")
	}
}

// TestUnwireDryRunSimulatesOrder: a dry run previews an older change to a
// file against what the newer undo would leave, so two changes to one
// file are not reported as drift.
func TestUnwireDryRunSimulatesOrder(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "c.toml")
	must(t, os.WriteFile(file, []byte("k = 1\n"), 0o600))
	apply(t, f, "r1", file, "a = 2\n")
	apply(t, f, "r2", file, "b = 3\n")
	var out bytes.Buffer
	undos, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{DryRun: true}, &out)
	if err != nil || len(undos) != 2 || undos[0].Outcome != Restored || undos[1].Outcome != Restored {
		t.Fatalf("dry run = %+v, %v\n%s", undos, err, out.String())
	}
	if !strings.Contains(out.String(), "-b = 3") || !strings.Contains(out.String(), "-a = 2") {
		t.Errorf("dry run output:\n%s", out.String())
	}
}

// TestUnwireDryRunFailedBeforeBackup: a failed modify that stopped before
// its backup previews as nothing to restore, not an error.
func TestUnwireDryRunFailedBeforeBackup(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	file := filepath.Join(f.dir, "c.toml")
	must(t, os.WriteFile(file, []byte("k = 1\n"), 0o600))
	c := apply(t, f, "r", file, "a = 2\n")
	// What a failure before the backup leaves: the file as before and a
	// failed row with no backup.
	must(t, os.WriteFile(file, []byte("k = 1\n"), 0o600))
	must(t, os.RemoveAll(filepath.Join(f.state, BackupDirName, c.ChangeID)))
	if _, err := f.s.DB().ExecContext(ctx, `UPDATE changes SET status = 'failed', backup_path = '' WHERE change_id = ?`, c.ChangeID); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		undos, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{DryRun: dry}, io.Discard)
		if err != nil || len(undos) != 1 || undos[0].Outcome != Cleaned {
			t.Errorf("dry=%v: Unwire = %+v, %v", dry, undos, err)
		}
	}
	if s := status(t, f, c.ChangeID); s != store.ChangeUndone {
		t.Errorf("status = %s", s)
	}
}
