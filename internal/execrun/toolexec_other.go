//go:build !unix

package execrun

import "errors"

// ExecTool is not supported on this OS yet.
func ExecTool(string, []string, string, []string) error {
	return errors.New("execrun: not supported on this OS yet")
}
