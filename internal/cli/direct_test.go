//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestRunDirect: the CLI runs with the socket live and named in its
// environment, and the socket is gone after.
func TestRunDirect(t *testing.T) {
	isolate(t)
	d, err := os.MkdirTemp("/tmp", "aggyd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	runDir := filepath.Join(d, "run")
	const parent = "11111111-2222-4333-8444-555555555555"
	c := adapter.Command{Path: "/bin/sh", Dir: d, Stdin: adapter.StdinTerminal, Env: []string{"AGGY_VENDOR=claude"},
		Args: []string{"-c", `test -S "$AGGY_SOCKET" && test "$AGGY_PARENT" = ` + parent + ` && test "$AGGY_VENDOR" = claude && exit 7`}}
	code, err := runDirect(c, runDir, parent, adapter.Claude)
	if code != 7 || err != nil {
		t.Errorf("runDirect = %d, %v", code, err)
	}
	if _, err := os.Lstat(filepath.Join(runDir, parent+".sock")); !os.IsNotExist(err) {
		t.Errorf("socket left behind: %v", err)
	}
	if code, _, _ := run("--direct"); code != 2 {
		t.Errorf("aggy --direct with no vendor: exit %d", code)
	}
}
