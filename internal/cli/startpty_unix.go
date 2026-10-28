//go:build unix

package cli

import (
	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/supervisor"
)

// startPTY adapts execrun.StartPTY to supervisor.Options.Start.
func startPTY(c adapter.Command, rows, cols int) (supervisor.Child, error) {
	p, err := execrun.StartPTY(c, rows, cols)
	if err != nil {
		return nil, err
	}
	return p, nil
}
