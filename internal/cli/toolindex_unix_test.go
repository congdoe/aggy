//go:build unix

package cli

import (
	"os"
	"os/exec"
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
			// The real init would say "already initialized" and exit 0 over
			// the half-built index: the next on rebuilds it instead.
			fakeCodegraph(t, "0")
			cg := filepath.Join(repo, ".codegraph")
			if code, _, errOut := toolRun("", false, "on", "codegraph", "--repo", repo); code != 0 || lines(t, filepath.Join(cg, "inits")) != 1 ||
				lines(t, filepath.Join(cg, "indexes")) != 1 || !exists(t, filepath.Join(cg, indexMarker)) {
				t.Fatalf("again: %d %q", code, errOut)
			}
		})
	}
}

// TestToolOnPromptInterrupt (T6711 F2): Ctrl-C at aggy tool on's
// first-index question ends aggy at once: no signal is caught while it
// waits for an answer.
func TestToolOnPromptInterrupt(t *testing.T) {
	if os.Getenv("AGGY_TEST_PROMPT_HELPER") != "" {
		isolate(t)
		fakeCodegraph(t, "0")
		repo := switchRepo(t, "")
		os.Exit(toolWith([]string{"on", "codegraph", "--repo", repo}, os.Stdin, true, os.Stdout, os.Stderr))
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestToolOnPromptInterrupt$")
	cmd.Env = append(os.Environ(), "AGGY_TEST_PROMPT_HELPER=1")
	stdin, err := cmd.StdinPipe() // held open: the answer never comes
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stdin.Close() }()
	var out lockedBuffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for deadline := time.Now().Add(2 * time.Minute); !strings.Contains(out.String(), "[y/N]"); time.Sleep(25 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("exited (%v) before asking:\n%s", err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("never asked:\n%s", out.String())
		}
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	select {
	case err := <-done:
		t.Logf("aggy tool on ended: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatalf("Ctrl-C at the prompt did not end aggy tool on:\n%s", out.String())
	}
}
