//go:build unix

package cli

import (
	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/supervisor"
)

// runDirect is aggy --direct: c runs on aggy's own terminal, with no
// pseudo-terminal, raw mode, switch key, or picker, so nothing of aggy's
// sits between the CLI and the terminal. The supervisor socket still
// listens and the index is still kept through hooks, as under aggy run.
func runDirect(c adapter.Command, runDir, parentID string, v adapter.Vendor) (int, error) {
	onHook, stopIndex := startIndex(parentID, v)
	defer stopIndex()
	srv, err := supervisor.Listen(runDir, parentID, nil, onHook)
	if err != nil {
		return 1, err
	}
	defer func() { _ = srv.Close() }()
	c.Env = supervisor.SocketEnv(c.Env, srv.Path(), parentID)
	return execrun.RunDirect(c)
}
