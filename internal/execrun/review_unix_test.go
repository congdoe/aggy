//go:build unix

package execrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestRunnerKillsProcessGroup (S12): a CLI's own child dies with it at the
// timeout.
func TestRunnerKillsProcessGroup(t *testing.T) {
	f := setup(t)
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	// The hook is the fake's child: it records its pid and sleeps.
	sc := `{"hooks":[{"event":"Stop","payload":{}}],
		"hook_command":["/bin/sh","-c","echo $$ > ` + pidFile + `; exec sleep 30","sh"]}`
	run := f.runner(t, sc, 2*time.Second)
	_, err := run(context.Background(), adapter.Command{Path: f.fakes.Claude, Args: []string{"-p", "x"}})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("run = %v, want ErrTimeout", err)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the grandchild never started: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("grandchild %d outlived the timeout", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
