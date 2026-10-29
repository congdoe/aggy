//go:build unix

package execrun

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"syscall"

	"github.com/congdoe/aggy/internal/safefs"
)

// ExecTool replaces aggy tool exec with a managed tool's MCP server
// (W71-2): it moves to dir, the launch's private folder, which must still
// be the directory want describes (the caller's Lstat of it, F74-1:
// safefs.ChdirNoFollow), and execs path (a clean absolute path) with args
// and exactly env (ToolEnv). The vendor CLI's stdin, stdout, and stderr,
// the server's MCP channel, pass through unchanged, and the server keeps
// the pid the CLI started, so the CLI's own stop reaches it. It returns
// only on failure.
func ExecTool(path string, args []string, dir string, want fs.FileInfo, env []string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("execrun: %q is not a clean absolute path", path)
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("execrun: working directory %q is not absolute", dir)
	}
	if err := safefs.Check(path); err != nil {
		return err
	}
	beforeChdir()
	if err := safefs.ChdirNoFollow(dir, want); err != nil {
		return err
	}
	return syscall.Exec(path, append([]string{path}, args...), env)
}

// beforeChdir runs between the caller's Lstat of dir and the move; a test
// swaps the folder here.
var beforeChdir = func() {}
