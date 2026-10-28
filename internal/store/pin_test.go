package store

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/congdoe/aggy/internal/safefs"
)

// pinOpeners are every way the store opens aggy.db or diag.db.
var pinOpeners = []struct {
	name, file string
	open       func(ctx context.Context, path string) (io.Closer, error)
}{
	{"Open", FileName, func(ctx context.Context, p string) (io.Closer, error) { return Open(ctx, p) }},
	{"OpenExisting", FileName, func(ctx context.Context, p string) (io.Closer, error) { return OpenExisting(ctx, p) }},
	{"OpenReadOnly", FileName, func(ctx context.Context, p string) (io.Closer, error) { return OpenReadOnly(ctx, p) }},
	{"OpenDiag", DiagFileName, func(ctx context.Context, p string) (io.Closer, error) { return OpenDiag(ctx, p) }},
	{"OpenDiagView", DiagFileName, func(ctx context.Context, p string) (io.Closer, error) { return OpenDiagView(ctx, p, "") }},
}

// makeDB creates a migrated, closed database of file's kind at path.
func makeDB(t *testing.T, file, path string) {
	t.Helper()
	ctx := context.Background()
	var c io.Closer
	var err error
	if file == DiagFileName {
		c, err = OpenDiag(ctx, path)
	} else {
		c, err = Open(ctx, path)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestOpenRefusesSymlink (T6777): a symlinked aggy.db or diag.db is
// refused by every opener, and Open does not create the link's target.
func TestOpenRefusesSymlink(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, o := range pinOpeners {
		t.Run(o.name, func(t *testing.T) {
			dir := t.TempDir()
			real := filepath.Join(dir, "real", o.file)
			makeDB(t, o.file, real)
			path := filepath.Join(dir, "state", o.file)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, path); err != nil {
				t.Fatal(err)
			}
			c, err := o.open(context.Background(), path)
			if err == nil {
				_ = c.Close()
			}
			if !errors.Is(err, safefs.ErrNotRegular) {
				t.Errorf("open through a symlink = %v, want ErrNotRegular", err)
			}
		})
	}
	t.Run("Open dangling", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "elsewhere.db")
		path := filepath.Join(dir, FileName)
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		if s, err := Open(context.Background(), path); !errors.Is(err, safefs.ErrNotRegular) {
			if err == nil {
				_ = s.Close()
			}
			t.Errorf("Open(dangling link) = %v, want ErrNotRegular", err)
		}
		if _, err := os.Lstat(target); err == nil {
			t.Error("Open created the symlink's target")
		}
	})
}

// TestOpenRefusesSwap (T6777): a file swapped between the pin and
// SQLite's open, for a symlink to the original or for a new file, is
// refused by every opener.
func TestOpenRefusesSwap(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	swaps := []struct {
		name string
		swap func(path string) error
		want error
	}{
		{"symlink", func(path string) error {
			if err := os.Rename(path, path+".moved"); err != nil {
				return err
			}
			return os.Symlink(path+".moved", path)
		}, safefs.ErrNotRegular},
		{"new file", func(path string) error {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if err := os.Rename(path, path+".moved"); err != nil {
				return err
			}
			return os.WriteFile(path, b, 0o600)
		}, safefs.ErrChanged},
	}
	for _, o := range pinOpeners {
		for _, sw := range swaps {
			t.Run(o.name+"/"+sw.name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), o.file)
				makeDB(t, o.file, path)
				done := false
				testHookBeforeOpen = func() {
					if !done {
						done = true
						if err := sw.swap(path); err != nil {
							t.Error(err)
						}
					}
				}
				t.Cleanup(func() { testHookBeforeOpen = nil })
				c, err := o.open(context.Background(), path)
				if err == nil {
					_ = c.Close()
				}
				if !done {
					t.Fatal("the hook never ran")
				}
				if !errors.Is(err, sw.want) {
					t.Errorf("open after a swap = %v, want %v", err, sw.want)
				}
			})
		}
	}
}
