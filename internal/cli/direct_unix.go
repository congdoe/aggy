//go:build unix

package cli

import (
	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/supervisor"
)

// runDirect is aggy --direct: c runs on aggy's own terminal, with no
// pseudo-terminal, raw mode, switch key, or picker, so nothing of aggy's
// sits between the CLI and the terminal. The supervisor socket still
// listens and the index is still kept through hooks, as under aggy run.
func runDirect(c adapter.Command, runDir, parentID string, v adapter.Vendor, assigned string, onID func(string), log func(diag.Rec), started func(pid int)) (int, error) {
	onHook, stopIndex := startIndex(parentID, v, assigned, onID)
	defer stopIndex()
	onHook, stopLimits := startLimits(parentID, v, onHook, nil)
	defer stopLimits()
	srv, err := supervisor.Listen(runDir, parentID, nil, onHook)
	if err != nil {
		return 1, err
	}
	defer func() { _ = srv.Close() }()
	srv.SetLog(log)
	c.Env = supervisor.SocketEnv(c.Env, srv.Path(), parentID)
	return execrun.RunDirectStarted(c, func(pid int) {
		srv.SetRoot(pid)
		started(pid)
	})
}
