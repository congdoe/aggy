//go:build !unix

package execrun

import (
	"errors"
	"io/fs"
)

// ExecTool is not supported on this OS yet.
func ExecTool(string, []string, string, fs.FileInfo, []string) error {
	return errors.New("execrun: not supported on this OS yet")
}
