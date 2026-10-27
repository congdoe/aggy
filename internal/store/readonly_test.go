package store

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
)

func TestOpenReadOnly(t *testing.T) {
	s, path := openTemp(t)
	_ = s.Close()
	ctx := context.Background()
	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	if v, err := ro.Version(ctx); err != nil || v < 1 {
		t.Errorf("Version = %d, %v", v, err)
	}
	if _, err := ro.DB().ExecContext(ctx, `INSERT INTO events (at, type) VALUES (1, 'x')`); err == nil {
		t.Error("a read-only store accepted a write")
	}
	missing := filepath.Join(t.TempDir(), FileName)
	if _, err := OpenReadOnly(ctx, missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenReadOnly(missing) = %v, want ErrNotExist", err)
	}
}
