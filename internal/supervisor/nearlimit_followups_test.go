//go:build unix

package supervisor

import (
	"sync"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestNearLimitResetJitter (T6811, L2): the same window read again with
// its reset a second sooner is not a new crossing; a window of another
// length, or one resetting more than resetSlack sooner, is.
func TestNearLimitResetJitter(t *testing.T) {
	checked := countChecks(t)
	var mu sync.Mutex
	base := time.Now().Add(time.Hour).Truncate(time.Second)
	cur := Share{Used: 0.95, Window: "300m@a", Minutes: 300, Reset: base}
	set := func(sh Share) { mu.Lock(); cur = sh; mu.Unlock() }
	used := func(adapter.Vendor) (Share, bool) { mu.Lock(); defer mu.Unlock(); return cur, true }
	ts, c, done, sock := switchRunOn(t, false, adapter.Codex, func(o *Options) {
		o.WarnAt, o.Used = 0.9, used
		o.HasQuota = func(v adapter.Vendor) bool { return v == adapter.Grok }
	})
	after := func(mark int) func() []byte { return func() []byte { return ts.screen()[mark:] } }
	dismiss := func(mark int) {
		t.Helper()
		ts.typeIn(t, "\x1b")
		poll(t, "the line cleared", []byte("\x1b7\x1b[24;1H\x1b[2K\x1b8"), after(mark))
	}
	_ = request(t, sock, codexStop)
	poll(t, "the warning", []byte("codex is at 95% of its limit"), ts.screen)
	dismiss(0)

	mark := len(ts.screen())
	set(Share{Used: 0.96, Window: "300m@b", Minutes: 300, Reset: base.Add(-time.Second)})
	_ = request(t, sock, codexStop)
	checked(2)
	never(t, "a warning for the same window a second sooner", []byte("of its limit"), after(mark), 200*time.Millisecond)

	set(Share{Used: 0.97, Window: "300m@c", Minutes: 300, Reset: base.Add(-2 * time.Minute)})
	_ = request(t, sock, codexStop)
	poll(t, "a warning for a window two minutes sooner", []byte("codex is at 97% of its limit"), after(mark))
	dismiss(mark)

	mark = len(ts.screen())
	set(Share{Used: 0.98, Window: "60m@d", Minutes: 60, Reset: base.Add(-2*time.Minute - time.Second)})
	_ = request(t, sock, codexStop)
	poll(t, "a warning for a shorter window", []byte("codex is at 98% of its limit"), after(mark))
	finish(t, c, done)
}

// TestNearLimitStuckRead (T6811, L3): a share read that never returns
// does not stop later warnings: past shareStale a new ask starts its own
// read, and the stuck one's answer, when it lands, is dropped.
func TestNearLimitStuckRead(t *testing.T) {
	old := shareStale
	shareStale = 0
	t.Cleanup(func() { shareStale = old })
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	var mu sync.Mutex
	calls := 0
	used := func(adapter.Vendor) (Share, bool) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			<-gate // stuck, as on a hung file system
			return Share{Used: 0.99, Window: "300m@old", Minutes: 300, Reset: time.Now().Add(time.Hour)}, true
		}
		return Share{Used: 0.95, Window: "300m@1", Minutes: 300, Reset: time.Now().Add(time.Hour)}, true
	}
	ts, c, done, sock := switchRunOn(t, false, adapter.Codex, func(o *Options) { o.WarnAt, o.Used = 0.9, used })
	_ = request(t, sock, codexStop)
	deadline := time.Now().Add(waitLimit)
	for {
		mu.Lock()
		n := calls
		mu.Unlock()
		if n >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the first read never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = request(t, sock, codexStop)
	poll(t, "the warning past a stuck read", []byte("codex is at 95% of its limit"), ts.screen)
	ts.typeIn(t, "\x1b")
	poll(t, "the line cleared", []byte("\x1b7\x1b[24;1H\x1b[2K\x1b8"), ts.screen)
	mark := len(ts.screen())
	release()
	never(t, "the stuck read's answer", []byte("of its limit"), func() []byte { return ts.screen()[mark:] }, 300*time.Millisecond)
	finish(t, c, done)
}
