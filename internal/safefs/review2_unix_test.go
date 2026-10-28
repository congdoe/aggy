//go:build unix

package safefs

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
)

// TestReadFileLimitFIFO (S9): a FIFO is not a regular file. Unix only:
// syscall.Mkfifo does not exist on Windows.
func TestReadFileLimitFIFO(t *testing.T) {
	fakeHome(t)
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	if _, err := ReadFileLimit(fifo, 100); !errors.Is(err, ErrNotRegular) {
		t.Errorf("fifo = %v, want ErrNotRegular", err)
	}
}
