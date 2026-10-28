//go:build !unix

package execrun

import (
	"errors"

	"github.com/congdoe/aggy/internal/adapter"
)

// RunDirect is not supported on this OS yet.
func RunDirect(adapter.Command) (int, error) {
	return 1, errors.New("execrun: not supported on this OS yet")
}
