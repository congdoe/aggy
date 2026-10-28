//go:build !unix

package cli

import (
	"fmt"
	"runtime"
)

// acceptOS refuses here: accept writes its patch in a private dir under
// aggy's state dir (supervisor.PrivateDir), which is Unix-only for now.
func acceptOS() error {
	return fmt.Errorf("aggy accept is not supported on %s yet; review the worktree and apply its diff by hand", runtime.GOOS)
}
