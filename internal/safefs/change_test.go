package safefs

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestFileChange: where the OS reports a change time, it moves on a chmod,
// which leaves the content and the modification time alone.
func TestFileChange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	before, ok := FileChange(fi)
	switch runtime.GOOS {
	case "darwin", "linux", "freebsd", "netbsd":
		if !ok || before <= 0 {
			t.Fatalf("no change time: %d %v", before, ok)
		}
	default:
		if ok {
			t.Fatalf("a change time where none is expected: %d", before)
		}
		return
	}
	// Set the times back, so the chmod's own stamp is later whatever the
	// clock's grain.
	old := time.Unix(1_000_000_000, 0)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	fi, _ = Lstat(p)
	mid, _ := FileChange(fi)
	if err := os.Chmod(p, 0o400); err != nil {
		t.Fatal(err)
	}
	fi, _ = Lstat(p)
	after, _ := FileChange(fi)
	if after < mid || !fi.ModTime().Equal(old) {
		t.Errorf("change time %d after %d, mtime %v", after, mid, fi.ModTime())
	}
}
