package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestReadApprovals (PR #129 review): a read pre-approval's identity is
// stored by path, replaced, kept as all 64 bits, and forgotten.
func TestReadApprovals(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	const p = "/repo/certs/server.key"
	for _, tt := range []struct {
		dev, ino uint64
	}{{1, 2}, {1<<63 + 5, 1<<64 - 1}} {
		if err := s.PutReadApproval(ctx, ReadApproval{Path: p, Dev: tt.dev, Ino: tt.ino, At: time.Now()}); err != nil {
			t.Fatal(err)
		}
		dev, ino, ok, err := s.ReadApprovalFor(ctx, p)
		if err != nil || !ok || dev != tt.dev || ino != tt.ino {
			t.Errorf("got %d %d %v %v, want %d %d", dev, ino, ok, err, tt.dev, tt.ino)
		}
	}
	if err := s.DeleteReadApproval(ctx, p); err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := s.ReadApprovalFor(ctx, p); ok || err != nil {
		t.Errorf("after delete: %v %v", ok, err)
	}
}
