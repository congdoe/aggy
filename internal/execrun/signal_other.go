//go:build !unix

package execrun

import (
	"context"
	"os"
	"os/signal"
)

// NotifyContext returns a context canceled on os.Interrupt.
func NotifyContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt)
}
