//go:build unix

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/safefs"
)

// TestOpenRefusesJournalFIFO (T6777 D64-2): a FIFO at aggy.db-journal or
// diag.db-journal is refused by every opener before SQLite's hot-journal
// open could block on it.
func TestOpenRefusesJournalFIFO(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, o := range pinOpeners {
		t.Run(o.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), o.file)
			makeDB(t, o.file, path)
			if err := syscall.Mkfifo(path+"-journal", 0o600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				c, err := o.open(context.Background(), path)
				if err == nil {
					_ = c.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, safefs.ErrNotRegular) {
					t.Errorf("open with a FIFO journal = %v, want ErrNotRegular", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("open hung on a FIFO journal")
			}
		})
	}
}

// TestSidecarSymlinkAfterConfirm (T6777 D64-3): the openers that open
// -wal and -shm only at the first query check them before, not after. A
// symlinked -wal planted after the check is refused by SQLite itself
// (SQLITE_CANTOPEN), and the link's target is never written.
func TestSidecarSymlinkAfterConfirm(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, o := range pinOpeners {
		switch o.name {
		case "OpenReadOnly", "OpenExisting":
		default:
			continue
		}
		t.Run(o.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, o.file)
			makeDB(t, o.file, path)
			decoy := filepath.Join(t.TempDir(), "decoy")
			if err := os.WriteFile(decoy, []byte("decoy"), 0o600); err != nil {
				t.Fatal(err)
			}
			planted := false
			testHookAfterConfirm = func() {
				if !planted {
					planted = true
					if err := os.Symlink(decoy, path+"-wal"); err != nil {
						t.Error(err)
					}
				}
			}
			t.Cleanup(func() { testHookAfterConfirm = nil })
			c, err := o.open(context.Background(), path)
			if err == nil {
				_ = c.Close()
			}
			if !planted {
				t.Fatal("the hook never ran")
			}
			if err == nil || !strings.Contains(err.Error(), "unable to open") {
				t.Errorf("open with a symlinked -wal planted after the check = %v, want SQLite CANTOPEN", err)
			}
			if b, rerr := os.ReadFile(decoy); rerr != nil || string(b) != "decoy" {
				t.Errorf("the decoy changed: %q %v", b, rerr)
			}
		})
	}
}
