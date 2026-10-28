//go:build !unix

package cli

import "errors"

// trustedPath cannot check owners here; there are no hooks to install
// until the Windows port.
func trustedPath(string) error { return errors.New("hooks are not supported on this OS yet") }
