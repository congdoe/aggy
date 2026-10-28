//go:build !unix

package supervisor

import "errors"

// Run needs a Unix terminal; Windows needs ConPTY (after 1.0).
func Run(Options) (Exit, error) {
	return Exit{Code: 1}, errors.New("supervisor: not supported on this OS yet")
}

// PrivateDir is not supported here: nothing runs a supervisor.
func PrivateDir(string) error { return errors.New("supervisor: not supported on this OS yet") }

// Live is false here: no supervisor runs on this OS.
func Live(string, string) (bool, error) { return false, nil }
