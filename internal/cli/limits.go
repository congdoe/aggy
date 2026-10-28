package cli

import (
	"context"
	"sync"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/limits"
	"github.com/congdoe/aggy/internal/notify"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
)

// codexLimitPoll is how often the session's Codex rollouts are checked
// for a limit.
var codexLimitPoll = 5 * time.Second

// hintEvery bounds how often a hook-only limit is recorded: a process
// the CLI started can send hook events at will, and each is a store row.
const hintEvery = time.Minute

// sessionLimits records the plan limits the on-screen CLI reports (see
// internal/limits).
type sessionLimits struct {
	parentID string
	vendor   adapter.Vendor
	// record writes one signal; tests replace it.
	record func(limits.Signal)
	// notifier shows desktop notifications; nil shows none.
	notifier *notify.Notifier

	mu       sync.Mutex
	lastHint time.Time
}

// startLimits wraps next, the session's other OnHook, so StopFailure
// rate_limit events are recorded too and Notification events notify
// (when the user config turns desktop notifications on), and for Codex
// starts the rollout watch. stop ends the watch once the CLI has exited.
func startLimits(parentID string, v adapter.Vendor, next func(supervisor.Request)) (onHook func(supervisor.Request), stop func()) {
	cfg, _ := config.LoadDefault() // an unreadable config leaves notifications off
	l := &sessionLimits{parentID: parentID, vendor: v, notifier: newNotifier(cfg)}
	l.record = l.save
	done := make(chan struct{})
	if v == adapter.Codex {
		if dir, err := codexSessionsDir(); err == nil {
			w := &limits.CodexWatch{SessionsDir: dir, Start: time.Now(), Found: l.record}
			go w.Run(codexLimitPoll, done)
		}
	}
	return func(r supervisor.Request) {
		if next != nil {
			next(r)
		}
		l.hook(r)
	}, func() { close(done) }
}

// hook records a StopFailure rate_limit from the vendor on screen, at
// most once per hintEvery, and notifies a Notification event (the CLI
// waits for the person, for example on an approval). Like every hook
// event, the latter is a hint; its text is aggy's own.
func (l *sessionLimits) hook(r supervisor.Request) {
	if r.Vendor != string(l.vendor) {
		return
	}
	if r.Event == "Notification" && l.notifier != nil {
		go l.notifier.Show(context.Background(), "waiting", "aggy", waitingText(l.vendor))
		return
	}
	sig, ok := limits.FromHook(l.vendor, r.Event, r.Payload)
	if !ok {
		return
	}
	l.mu.Lock()
	now := time.Now()
	if now.Sub(l.lastHint) < hintEvery {
		l.mu.Unlock()
		return
	}
	l.lastHint = now
	l.mu.Unlock()
	l.record(sig)
}

// save opens the index and records sig. A failed write loses one signal;
// it must not disturb the CLI.
func (l *sessionLimits) save(sig limits.Signal) {
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		return
	}
	defer func() { _ = s.Close() }()
	until, err := limits.Record(ctx, s, sig, l.parentID, time.Now())
	if err == nil && l.notifier != nil {
		go showLimit(ctx, l.notifier, sig, until)
	}
}
