package cli

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/limits"
	"github.com/congdoe/aggy/internal/notify"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
)

// notifyEvery is the least time between two notifications of one kind,
// so a process that floods hook events cannot flood the desktop.
const notifyEvery = 30 * time.Second

// notifyTimeout bounds the notifier tool.
const notifyTimeout = 5 * time.Second

// newNotifier returns the desktop notifier when the user config turns it
// on and the tool is installed, else nil (which shows nothing).
func newNotifier(cfg config.Config) *notify.Notifier {
	if !cfg.Notify.Desktop {
		return nil
	}
	path := ""
	if runtime.GOOS == "darwin" {
		if fi, err := safefs.Stat(notify.Osascript); err == nil && fi.Mode().IsRegular() {
			path = notify.Osascript
		}
	} else {
		path, _ = probe.LookPath("notify-send")
	}
	if path == "" {
		return nil
	}
	return &notify.Notifier{Run: execrun.NotifyRunner(notifyTimeout), GOOS: runtime.GOOS, Path: path, Every: notifyEvery}
}

// vendorTitle is v's name as a notification shows it.
func vendorTitle(v adapter.Vendor) string {
	s := string(v)
	if s == "" {
		return "A vendor"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Notification texts. They hold vendor names, borrow kinds, outcome
// classes, and times only: never a session id, a path, an environment
// value, or text a vendor or model wrote.

func waitingText(v adapter.Vendor) string { return vendorTitle(v) + " is waiting for you." }

func limitText(sig limits.Signal, until time.Time) string {
	what := "hit its usage limit"
	if sig.Via == limits.ViaHook {
		what = "reported a usage limit"
	}
	return fmt.Sprintf("%s %s; aggy expects it back about %s.", vendorTitle(sig.Vendor), what, until.Local().Format("Jan 2 15:04"))
}

func borrowText(kind adapter.Kind, v adapter.Vendor, outcome string) string {
	return fmt.Sprintf("The %s borrow on %s finished: %s.", kind, vendorTitle(v), outcome)
}

// showLimit notifies a recorded limit, once per vendor per notifyEvery.
func showLimit(ctx context.Context, n *notify.Notifier, sig limits.Signal, until time.Time) {
	n.Show(ctx, "limit-"+string(sig.Vendor), "aggy", limitText(sig, until))
}
