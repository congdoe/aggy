package patch

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/congdoe/aggy/internal/safefs"
)

const line = "project_doc_fallback_filenames = [\"CLAUDE.md\"]\n"

// addLine appends line unless the content already has it.
func addLine(b []byte) ([]byte, error) {
	if bytes.Contains(b, []byte(line)) {
		return b, nil
	}
	return append(append([]byte{}, b...), line...), nil
}

type env struct {
	dir, file, backups string
}

func setup(t *testing.T, content *string) env {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	e := env{dir: dir, file: filepath.Join(dir, "config.toml"), backups: filepath.Join(dir, "backups")}
	if content != nil {
		if err := os.WriteFile(e.file, []byte(*content), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func ptr(s string) *string { return &s }

func content(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApply(t *testing.T) {
	orig := "model = \"o\"\n"
	e := setup(t, ptr(orig))
	p := Patch{File: e.file, ExpectedHash: Hash([]byte(orig)), Transform: addLine}

	rec, err := Apply(p, e.backups)
	if err != nil {
		t.Fatal(err)
	}
	want := orig + line
	if got := content(t, e.file); got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if !rec.Changed || rec.BeforeHash != Hash([]byte(orig)) || rec.AfterHash != Hash([]byte(want)) {
		t.Errorf("record = %+v", rec)
	}
	if got := content(t, rec.BackupPath); got != orig {
		t.Errorf("backup = %q, want %q", got, orig)
	}
	fi, err := os.Stat(e.file)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o, want 0640 kept", fi.Mode().Perm())
	}
}

func TestReapplyIsNoOp(t *testing.T) {
	orig := "a = 1\n"
	e := setup(t, ptr(orig))
	p := Patch{File: e.file, ExpectedHash: Hash([]byte(orig)), Transform: addLine}
	if _, err := Apply(p, e.backups); err != nil {
		t.Fatal(err)
	}
	fi1, _ := os.Stat(e.file)
	rec, err := Apply(p, e.backups)
	if err != nil {
		t.Fatalf("re-apply: %v", err)
	}
	if rec.Changed || rec.BackupPath != "" {
		t.Errorf("re-apply record = %+v, want unchanged", rec)
	}
	fi2, _ := os.Stat(e.file)
	if !fi1.ModTime().Equal(fi2.ModTime()) {
		t.Error("re-apply rewrote the file")
	}
	if got := content(t, e.file); got != orig+line {
		t.Errorf("content = %q", got)
	}
}

func TestDriftRefused(t *testing.T) {
	e := setup(t, ptr("edited by the user\n"))
	p := Patch{File: e.file, ExpectedHash: Hash([]byte("a = 1\n")), Transform: addLine}

	_, err := Apply(p, e.backups)
	if !errors.Is(err, ErrDrift) {
		t.Fatalf("Apply = %v, want ErrDrift", err)
	}
	var de *DriftError
	if !errors.As(err, &de) || de.Actual != Hash([]byte("edited by the user\n")) {
		t.Errorf("DriftError = %+v", de)
	}
	if got := content(t, e.file); got != "edited by the user\n" {
		t.Errorf("file changed: %q", got)
	}
	if _, err := os.Stat(e.backups); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("backup dir created on drift: %v", err)
	}
}

func TestCreateNewFile(t *testing.T) {
	e := setup(t, nil)
	file := filepath.Join(e.dir, "sub", "new.toml")
	p := Patch{File: file, ExpectedHash: NoFile, Transform: addLine}

	rec, err := Apply(p, e.backups)
	if err != nil {
		t.Fatal(err)
	}
	if rec.BeforeHash != NoFile || rec.BackupPath != "" || !rec.Changed {
		t.Errorf("record = %+v", rec)
	}
	if got := content(t, file); got != line {
		t.Errorf("content = %q", got)
	}
	fi, _ := os.Stat(file)
	if fi.Mode().Perm() != DefaultMode {
		t.Errorf("mode = %o, want %o", fi.Mode().Perm(), DefaultMode)
	}

	// Re-applying a create is a no-op.
	if rec, err := Apply(p, e.backups); err != nil || rec.Changed {
		t.Errorf("re-apply = %+v, %v", rec, err)
	}

	// A file that appeared with other content is drift.
	other := filepath.Join(e.dir, "other.toml")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(Patch{File: other, ExpectedHash: NoFile, Transform: addLine}, e.backups); !errors.Is(err, ErrDrift) {
		t.Errorf("Apply on existing file = %v, want ErrDrift", err)
	}

	// A file expected to exist but missing is drift.
	gone := filepath.Join(e.dir, "gone.toml")
	if _, err := Apply(Patch{File: gone, ExpectedHash: Hash([]byte("x")), Transform: addLine}, e.backups); !errors.Is(err, ErrDrift) {
		t.Errorf("Apply on missing file = %v, want ErrDrift", err)
	}
}

func TestPlanWritesNothing(t *testing.T) {
	orig := "a = 1\n"
	e := setup(t, ptr(orig))
	tests := []struct {
		name string
		p    Patch
		want Action
	}{
		{"modify", Patch{File: e.file, ExpectedHash: Hash([]byte(orig)), Transform: addLine}, Modify},
		{"create", Patch{File: filepath.Join(e.dir, "new.toml"), ExpectedHash: NoFile, Transform: addLine}, Create},
		{"noop", Patch{File: e.file, ExpectedHash: Hash([]byte(orig)), Transform: func(b []byte) ([]byte, error) { return b, nil }}, NoOp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pl, err := Plan(tt.p)
			if err != nil {
				t.Fatal(err)
			}
			if pl.Action != tt.want {
				t.Errorf("action = %s, want %s", pl.Action, tt.want)
			}
			if pl.AfterHash != Hash(pl.After) {
				t.Error("AfterHash does not match After")
			}
		})
	}
	if got := content(t, e.file); got != orig {
		t.Errorf("Plan changed the file: %q", got)
	}
	entries, _ := os.ReadDir(e.dir)
	if len(entries) != 1 {
		t.Errorf("Plan created files: %v", entries)
	}
	if _, err := Plan(Patch{File: e.file, ExpectedHash: "bad", Transform: addLine}); !errors.Is(err, ErrDrift) {
		t.Errorf("Plan drift = %v, want ErrDrift", err)
	}
}

func TestRevert(t *testing.T) {
	t.Run("modify", func(t *testing.T) {
		orig := "a = 1\n"
		e := setup(t, ptr(orig))
		rec, err := Apply(Patch{File: e.file, ExpectedHash: Hash([]byte(orig)), Transform: addLine}, e.backups)
		if err != nil {
			t.Fatal(err)
		}
		if err := Revert(rec); err != nil {
			t.Fatal(err)
		}
		if got := content(t, e.file); got != orig {
			t.Errorf("content = %q, want %q", got, orig)
		}
		fi, _ := os.Stat(e.file)
		if fi.Mode().Perm() != 0o640 {
			t.Errorf("mode = %o, want 0640", fi.Mode().Perm())
		}
		// Reverting twice is a no-op.
		if err := Revert(rec); err != nil {
			t.Errorf("second revert: %v", err)
		}
	})

	t.Run("create", func(t *testing.T) {
		e := setup(t, nil)
		rec, err := Apply(Patch{File: e.file, ExpectedHash: NoFile, Transform: addLine}, e.backups)
		if err != nil {
			t.Fatal(err)
		}
		if err := Revert(rec); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(e.file); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("created file not removed: %v", err)
		}
	})

	t.Run("refused after drift", func(t *testing.T) {
		orig := "a = 1\n"
		e := setup(t, ptr(orig))
		rec, err := Apply(Patch{File: e.file, ExpectedHash: Hash([]byte(orig)), Transform: addLine}, e.backups)
		if err != nil {
			t.Fatal(err)
		}
		edited := orig + line + "user = true\n"
		if err := os.WriteFile(e.file, []byte(edited), 0o640); err != nil {
			t.Fatal(err)
		}
		if err := Revert(rec); !errors.Is(err, ErrDrift) {
			t.Fatalf("Revert = %v, want ErrDrift", err)
		}
		if got := content(t, e.file); got != edited {
			t.Errorf("file changed: %q", got)
		}
	})

	t.Run("created file refused after drift", func(t *testing.T) {
		e := setup(t, nil)
		rec, err := Apply(Patch{File: e.file, ExpectedHash: NoFile, Transform: addLine}, e.backups)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.file, []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := Revert(rec); !errors.Is(err, ErrDrift) {
			t.Fatalf("Revert = %v, want ErrDrift", err)
		}
	})
}

func TestRefusesAuthPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	p := Patch{File: filepath.Join(home, ".codex", "auth.json"), ExpectedHash: NoFile, Transform: addLine}
	if _, err := Apply(p, filepath.Join(home, "b")); !errors.Is(err, safefs.ErrAuthPath) {
		t.Errorf("Apply = %v, want safefs.ErrAuthPath", err)
	}
	if _, err := Plan(p); !errors.Is(err, safefs.ErrAuthPath) {
		t.Errorf("Plan = %v, want safefs.ErrAuthPath", err)
	}
}

func TestPatchValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, err := Plan(Patch{File: "x", Transform: addLine}); err == nil {
		t.Error("missing ExpectedHash accepted")
	}
	if _, err := Plan(Patch{File: "x", ExpectedHash: NoFile}); err == nil {
		t.Error("missing Transform accepted")
	}
}
