//go:build !unix

package cli

import (
	"errors"

	"github.com/congdoe/aggy/internal/adapter"
)

// runDirect needs the supervisor socket, which is Unix-only for now.
func runDirect(adapter.Command, string, string, adapter.Vendor, string) (int, error) {
	return 1, errors.New("aggy --direct is not supported on this OS yet")
}
