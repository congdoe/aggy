//go:build !unix

package cli

import (
	"errors"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/supervisor"
)

// runDirect needs the supervisor socket, which is Unix-only for now.
func runDirect(adapter.Command, string, string, adapter.Vendor, string, string, func(string), func(supervisor.Request), func(diag.Rec), func(int)) (int, error) {
	return 1, errors.New("aggy --direct is not supported on this OS yet")
}
