//go:build !unix

package execrun

import (
	"errors"

	"github.com/congdoe/aggy/internal/adapter"
)

// PTY is unavailable here: the Windows port (after 1.0, design section 2)
// adds ConPTY.
type PTY struct{}

// StartPTY needs a Unix pseudo-terminal; Windows needs ConPTY (after 1.0).
func StartPTY(adapter.Command, int, int) (*PTY, error) {
	return nil, errors.New("execrun: pseudo-terminals are not supported on this OS yet")
}
