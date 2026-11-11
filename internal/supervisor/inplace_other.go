//go:build !unix

package supervisor

import (
	"errors"
	"os"
)

// InPlace is not supported here: a list drawn in place falls back to typed
// lines.
type InPlace struct{}

// StartInPlace refuses here, so the caller falls back to typed lines.
func StartInPlace(*os.File, func(), func()) (*InPlace, error) {
	return nil, errors.New("supervisor: not supported on this OS yet")
}

// Size is unknown here.
func (*InPlace) Size() (rows, cols int) { return 0, 0 }

// Restore does nothing here.
func (*InPlace) Restore() {}

// Interrupt exits as an interrupted command does.
func Interrupt() { os.Exit(130) }
