//go:build unix

package execrun

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// NotifyContext returns a context that is canceled when aggy gets
// SIGINT, SIGTERM, or SIGHUP: Ctrl-C, a kill, or the terminal (or the
// parent agent's shell tool) going away. A runner whose context ends
// kills the child's whole process group, so a borrow child never outlives
// an interrupted aggy borrow. Until stop is called, a second signal is
// caught too and does nothing more: aggy ends once the child is killed
// and the outcome recorded.
func NotifyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
}
