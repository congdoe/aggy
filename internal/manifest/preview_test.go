package manifest

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch\ngot:\n%s\nwant:\n%s", name, got, want)
	}
}

// patches returns a modify, a create, a no-op, and a drifted patch in dir.
func patches(t *testing.T, dir string) []patch.Patch {
	t.Helper()
	codex := filepath.Join(dir, "codex.toml")
	orig := "model = \"o\"\napproval_policy = \"on-request\"\n"
	must(t, os.WriteFile(codex, []byte(orig), 0o600))
	done := filepath.Join(dir, "done.toml")
	must(t, os.WriteFile(done, []byte(line), 0o600))
	drift := filepath.Join(dir, "drift.json")
	must(t, os.WriteFile(drift, []byte("{\"edited\": true}\n"), 0o600))
	return []patch.Patch{
		{File: codex, ExpectedHash: patch.Hash([]byte(orig)), Transform: addLine},
		{File: filepath.Join(dir, "grok", "settings.toml"), ExpectedHash: patch.NoFile, Transform: addLine},
		{File: done, ExpectedHash: patch.Hash([]byte("whatever")), Transform: addLine},
		{File: drift, ExpectedHash: patch.Hash([]byte("{}\n")), Transform: addLine},
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestPreviewGolden(t *testing.T) {
	f := setup(t)
	ps := patches(t, f.dir)
	var out bytes.Buffer
	n, err := Preview(&out, ps)
	if n != 2 {
		t.Errorf("would change %d files, want 2", n)
	}
	if !errors.Is(err, patch.ErrDrift) {
		t.Errorf("Preview error = %v, want drift", err)
	}
	golden(t, "preview.golden", strings.ReplaceAll(out.String(), f.dir, "$DIR"))

	// Nothing was written: not the files, not a backup, not a row.
	if _, err := os.Stat(filepath.Join(f.dir, "grok")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("preview created a directory: %v", err)
	}
	if b, _ := os.ReadFile(ps[0].File); strings.Contains(string(b), line) {
		t.Error("preview wrote a file")
	}
	if _, err := os.Stat(filepath.Join(f.state, BackupDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("preview made backups: %v", err)
	}
	if cs, _ := f.s.Changes(context.Background(), ""); len(cs) != 0 {
		t.Errorf("preview recorded %d changes", len(cs))
	}
}

func TestSetupMode(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	mode, err := SetupMode(ctx, false, false)
	if err != nil || mode != DryRun {
		t.Fatalf("first setup mode = %v, %v, want dry-run", mode, err)
	}
	if mode, _ := SetupMode(ctx, false, true); mode != Write {
		t.Errorf("--write = %v", mode)
	}
	if _, err := SetupMode(ctx, true, true); err == nil {
		t.Error("--dry-run --write accepted")
	}

	// The dry run writes nothing, so the next setup still previews.
	ps := patches(t, f.dir)[:2]
	var out bytes.Buffer
	cs, err := f.m.Run(ctx, mode, NewRunID(), ps, &out)
	if err != nil || cs != nil || !strings.Contains(out.String(), "+++ ") {
		t.Fatalf("dry run = %v, %v, %q", cs, err, out.String())
	}
	if mode, _ := SetupMode(ctx, false, false); mode != DryRun {
		t.Errorf("after a dry run, mode = %v", mode)
	}

	cs, err = f.m.Run(ctx, Write, NewRunID(), ps, &out)
	if err != nil || len(cs) != 2 || cs[0].Status != store.ChangeApplied || cs[1].Status != store.ChangeApplied {
		t.Fatalf("write run = %+v, %v", cs, err)
	}
	if mode, _ := SetupMode(ctx, false, false); mode != Write {
		t.Errorf("after a write, mode = %v, want write", mode)
	}
	if mode, _ := SetupMode(ctx, true, false); mode != DryRun {
		t.Errorf("--dry-run after a write = %v", mode)
	}
}
