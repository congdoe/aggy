//go:build unix

package execrun

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestRunKillsBackgroundChildren (H-2): a child the command leaves in the
// background dies when the command exits, and run does not wait for it.
func TestRunKillsBackgroundChildren(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pidFile := filepath.Join(t.TempDir(), "pid")
	start := time.Now()
	out, err := ExecRunner(10*time.Second)(context.Background(), adapter.Command{
		Path: "/bin/sh", Args: []string{"-c", `sleep 30 & echo $! > "$0"; echo done`, pidFile},
	})
	if err != nil || out.ExitCode != 0 || strings.TrimSpace(string(out.Stdout)) != "done" {
		t.Fatalf("run = %q exit %d, %v", out.Stdout, out.ExitCode, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("run waited %s for the background child", d)
	}
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 1 {
		t.Fatalf("pid %q", b)
	}
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the background sleep outlived its command")
		}
	}
}

// TestCheckRunnerKeepsTail (H-3): CheckRunner keeps the end of a long
// output, ExecRunner its start.
func TestCheckRunnerKeepsTail(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := adapter.Command{Path: "/bin/sh", Args: []string{"-c", "echo START; yes line | head -c 3000000; echo END"}}
	out, err := CheckRunner(10*time.Second)(context.Background(), c)
	if err != nil || len(out.Stdout) != MaxOutput || !bytes.HasSuffix(out.Stdout, []byte("line\nEND\n")) {
		t.Fatalf("tail: %d bytes, ends %q, %v", len(out.Stdout), out.Stdout[max(0, len(out.Stdout)-20):], err)
	}
	out, err = ExecRunner(10*time.Second)(context.Background(), c)
	if err != nil || len(out.Stdout) != MaxOutput || !bytes.HasPrefix(out.Stdout, []byte("START\n")) {
		t.Fatalf("head: %d bytes, %v", len(out.Stdout), err)
	}
}

func TestCappedTail(t *testing.T) {
	c := &capped{max: 4, tail: true}
	for _, s := range []string{"ab", "cdefghij", "k", "lm"} {
		if n, _ := c.Write([]byte(s)); n != len(s) {
			t.Fatalf("Write(%q) = %d", s, n)
		}
	}
	if got := string(c.bytes()); got != "jklm" {
		t.Errorf("tail = %q, want jklm", got)
	}
}
