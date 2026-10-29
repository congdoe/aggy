//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCodegraphInitInterrupted (T6711 H1): SIGINT, SIGTERM, or SIGHUP to
// aggy tool on while codegraph init runs kills init's process group and
// records nothing; the database init left has no marker, so the next on
// builds the index again.
func TestCodegraphInitInterrupted(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) {
			isolate(t)
			fakeCodegraph(t, "sleep")
			repo := switchRepo(t, "")
			pidFile := filepath.Join(repo, ".codegraph", "pid")
			go func() {
				for range 400 {
					if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
						_ = syscall.Kill(os.Getpid(), sig)
						return
					}
					time.Sleep(25 * time.Millisecond)
				}
			}()
			code, _, errOut := toolRun("", false, "on", "codegraph", "--repo", repo)
			if code != 1 || !strings.Contains(errOut, "interrupted") {
				t.Fatalf("code %d %q", code, errOut)
			}
			b, _ := os.ReadFile(pidFile)
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil {
				t.Fatal(err)
			}
			for deadline := time.Now().Add(3 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
				if time.Now().After(deadline) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatal("codegraph init outlived aggy tool on")
				}
			}
			codegraphOff(t, repo)
			if exists(t, filepath.Join(repo, ".codegraph", indexMarker)) {
				t.Error("marked as indexed")
			}
			fakeCodegraph(t, "0")
			if code, _, errOut := toolRun("", false, "on", "codegraph", "--repo", repo); code != 0 || lines(t, filepath.Join(repo, ".codegraph", "inits")) != 2 {
				t.Fatalf("again: %d %q", code, errOut)
			}
		})
	}
}
