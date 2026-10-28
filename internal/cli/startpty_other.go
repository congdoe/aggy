//go:build !unix

package cli

import (
	"errors"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/supervisor"
)

// startPTY is unavailable until the Windows port adds ConPTY.
func startPTY(adapter.Command, int, int) (supervisor.Child, error) {
	return nil, errors.New("pseudo-terminals are not supported on this OS yet")
}
