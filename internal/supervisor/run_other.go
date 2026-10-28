//go:build !unix

package supervisor

import "errors"

// Run needs a Unix terminal; Windows needs ConPTY (after 1.0).
func Run(Options) (int, error) {
	return 1, errors.New("supervisor: not supported on this OS yet")
}
