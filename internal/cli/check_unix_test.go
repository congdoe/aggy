//go:build unix

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestCheckInterrupt (H-1): Ctrl-C ends aggy check and kills the running
// check's process group.
func TestCheckInterrupt(t *testing.T) {
	checkTools(t, "sleep")
	pidFile := filepath.Join(t.TempDir(), "pid")
	root := checkRepo(t, `check = [["./wait.sh", "`+pidFile+`"]]`, map[string]string{
		"wait.sh": "echo $$ > \"$1\"\nexec sleep 777\n",
	})
	t.Chdir(root)
	go func() {
		for i := 0; i < 200; i++ {
			if b, err := os.ReadFile(pidFile); err == nil && len(b) > 0 {
				_ = syscall.Kill(os.Getpid(), syscall.SIGINT)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
	var out, errb bytes.Buffer
	start := time.Now()
	if code := runCheck(nil, &out, &errb); code != 1 || !strings.Contains(out.String(), "interrupted") {
		t.Fatalf("code %d\n%s%s", code, out.String(), errb.String())
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("interrupt took %s", d)
	}
	b, _ := os.ReadFile(pidFile)
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("the check outlived aggy check")
		}
	}
}
