//go:build unix

package supervisor

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// fakeUse is what Options.Used reports: used in thousandths (negative: no
// data) of window, which resets at reset. calls counts the reads.
type fakeUse struct {
	mu     sync.Mutex
	used   int64
	window string
	reset  time.Time
	calls  int
}

func newFakeUse(used int64) *fakeUse {
	return &fakeUse{used: used, window: "300@1", reset: time.Now().Add(time.Hour)}
}

func (f *fakeUse) set(used int64, window string, reset time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.used, f.window, f.reset = used, window, reset
}

func (f *fakeUse) read(adapter.Vendor) (Share, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return Share{Used: float64(f.used) / 1000, Window: f.window, Reset: f.reset}, f.used >= 0
}

// waitCalls waits until Used has been read n times in all (L60-6).
func (f *fakeUse) waitCalls(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		c := f.calls
		f.mu.Unlock()
		if c >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Used was not read %d times", n)
}

// usedOpts reports f as the share of the on-screen vendor's limit used,
// warns at warnAt, and gives grok quota.
func usedOpts(f *fakeUse, warnAt float64) func(*Options) {
	return func(o *Options) {
		o.WarnAt, o.Used = warnAt, f.read
		o.HasQuota = func(v adapter.Vendor) bool { return v == adapter.Grok }
	}
}

const (
	codexStop  = `{"type":"hook-event","vendor":"codex","event":"Stop","payload":{"session_id":"` + childSID + `"}}`
	codexOther = `{"type":"hook-event","vendor":"codex","event":"Stop","payload":{"session_id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}}`
)

// TestNearLimitWarn (T6699): a turn end of the on-screen child at or past
// warn_at shows one status line, once per crossing; below it, or for
// another session's turn end, nothing. A crossing ends on a lower reading
// of the same window or when its reset passes, not on a lower share from
// another window (L60-3). The switch key opens the list with the first
// vendor with quota preselected; nothing switches by itself, and unsent
// text still blocks the switch.
func TestNearLimitWarn(t *testing.T) {
	f := newFakeUse(500)
	ts, c, done, sock := switchRunOn(t, false, adapter.Codex, usedOpts(f, 0.9))
	const want = "aggy: codex is at 90% of its limit. Ctrl+\\ hands off to grok, Esc dismisses."
	after := func(mark int) func() []byte { return func() []byte { return ts.screen()[mark:] } }
	hour := time.Now().Add(time.Hour)

	_ = request(t, sock, codexStop) // below
	f.waitCalls(t, 1)
	never(t, "a warning below warn_at", []byte("of its limit"), ts.screen, 200*time.Millisecond)

	f.set(900, "300@1", hour) // exactly at warn_at
	_ = request(t, sock, codexStop)
	poll(t, "the warning", []byte("\x1b7\x1b[24;1H\x1b[2K\x1b[7m"+want+"\x1b[0m\x1b8"), ts.screen)
	ts.typeIn(t, "\x1b")
	poll(t, "the line cleared", []byte("\x1b7\x1b[24;1H\x1b[2K\x1b8"), ts.screen)

	f.set(950, "300@1", hour) // still past: once per crossing
	mark := len(ts.screen())
	_ = request(t, sock, codexStop)
	f.waitCalls(t, 3)
	never(t, "a second warning", []byte("of its limit"), after(mark), 200*time.Millisecond)

	f.set(300, "other", hour) // another source's lower share: no reset
	_ = request(t, sock, codexStop)
	f.waitCalls(t, 4)
	f.set(950, "300@1", hour)
	_ = request(t, sock, codexStop)
	f.waitCalls(t, 5)
	never(t, "a warning after another window's dip", []byte("of its limit"), after(mark), 200*time.Millisecond)

	f.set(300, "300@1", hour) // the same window below: the next crossing warns
	_ = request(t, sock, codexStop)
	f.waitCalls(t, 6)
	f.set(910, "300@1", time.Now().Add(300*time.Millisecond))
	_ = request(t, sock, codexOther)
	never(t, "a nested session's turn end", []byte("of its limit"), after(mark), 200*time.Millisecond)
	_ = request(t, sock, codexStop)
	poll(t, "the warning again", []byte("codex is at 91% of its limit"), after(mark))
	ts.typeIn(t, "\x1b")
	poll(t, "the line cleared", []byte("\x1b7\x1b[24;1H\x1b[2K\x1b8"), after(mark))

	time.Sleep(400 * time.Millisecond) // the warned window's reset passes
	f.set(920, "300@2", hour)
	mark = len(ts.screen())
	_ = request(t, sock, codexStop)
	poll(t, "the warning after the reset", []byte("codex is at 92% of its limit"), after(mark))

	ts.typeIn(t, "\x1c") // opens the list, grok preselected, brief on
	poll(t, "the preselect", []byte(" >2  grok"), ts.screen)
	poll(t, "brief on", []byte("b  brief: on"), ts.screen)
	if bytes.Contains(c.received(), []byte("/exit")) {
		t.Fatal("the warning switched by itself")
	}
	ts.typeIn(t, "\r")
	poll(t, "the list to close", []byte(altScreenOff), ts.screen)
	time.Sleep(60 * time.Millisecond)
	ts.typeIn(t, "draft")
	c.waitGot(t, []byte("draft"))
	_ = request(t, sock, codexStop)
	poll(t, "the unsent-text note", []byte("you typed text you have not sent"), ts.screen)
	if bytes.Contains(c.received(), []byte("/exit")) {
		t.Fatal("/exit typed over unsent text")
	}
	finish(t, c, done)
}

// TestNearLimitAndLimit (L60-1): a limit ends the warning in either
// order: one on screen gives way to the limit's note, and none follows
// the note for that crossing.
func TestNearLimitAndLimit(t *testing.T) {
	t.Run("warning, then limit", func(t *testing.T) {
		f := newFakeUse(990)
		ch := make(chan LimitHit, 1)
		ts, c, done, sock := switchRunOn(t, false, adapter.Codex, limitOpts(ch, adapter.Grok), usedOpts(f, 0.9))
		_ = request(t, sock, codexStop)
		poll(t, "the warning", []byte("codex is at 99% of its limit"), ts.screen)
		mark := len(ts.screen())
		ch <- LimitHit{Vendor: adapter.Codex}
		poll(t, "the limit note", []byte("codex hit its limit. grok has quota left"), func() []byte { return ts.screen()[mark:] })
		ts.typeIn(t, "\x1b")
		mark = len(ts.screen())
		_ = request(t, sock, codexStop)
		f.waitCalls(t, 2)
		never(t, "a warning after the limit", []byte("of its limit."), func() []byte { return ts.screen()[mark:] }, 300*time.Millisecond)
		finish(t, c, done)
	})
	t.Run("limit, then turn end", func(t *testing.T) {
		f := newFakeUse(1000)
		ch := make(chan LimitHit, 1)
		ts, c, done, sock := switchRunOn(t, false, adapter.Codex, limitOpts(ch, adapter.Grok), usedOpts(f, 0.9))
		_ = request(t, sock, codexStop) // the share is read; the limit lands before the quiet wait ends
		ch <- LimitHit{Vendor: adapter.Codex}
		poll(t, "the limit note", []byte("codex hit its limit. grok has quota left"), ts.screen)
		f.waitCalls(t, 1)
		time.Sleep(100 * time.Millisecond)
		mark := len(ts.screen())
		ts.typeIn(t, "\x1b")
		never(t, "a warning after the note", []byte("of its limit."), func() []byte { return ts.screen()[mark:] }, 300*time.Millisecond)
		finish(t, c, done)
	})
}

// TestNearLimitSilent (T6699): no warning without data (a vendor that
// writes no allowance locally) or with warn_at 0, which never reads it
// (L60-2).
func TestNearLimitSilent(t *testing.T) {
	for _, tt := range []struct {
		name      string
		used      int64
		warnAt    float64
		wantCalls int
	}{
		{"no local data", -1, 0.9, 1},
		{"turned off", 990, 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeUse(tt.used)
			ts, c, done, sock := switchRunOn(t, false, adapter.Claude, usedOpts(f, tt.warnAt))
			_ = request(t, sock, stopLine)
			never(t, "a warning", []byte("of its limit"), ts.screen, 300*time.Millisecond)
			f.mu.Lock()
			calls := f.calls
			f.mu.Unlock()
			if calls != tt.wantCalls {
				t.Errorf("Used read %d times, want %d", calls, tt.wantCalls)
			}
			finish(t, c, done)
		})
	}
}

// TestNearLimitNoQuota (T6699): with no other vendor left the warning
// says so; the switch key still opens the list, with nothing preselected.
func TestNearLimitNoQuota(t *testing.T) {
	f := newFakeUse(990)
	ts, c, done, sock := switchRunOn(t, false, adapter.Codex, usedOpts(f, 0.9), func(o *Options) {
		o.HasQuota = func(adapter.Vendor) bool { return false }
	})
	_ = request(t, sock, codexStop)
	poll(t, "the warning", []byte("aggy: codex is at 99% of its limit. No vendor with quota left; Esc dismisses."), ts.screen)
	ts.typeIn(t, "\x1c")
	poll(t, "the list", []byte("aggy: switch vendor"), ts.screen)
	if bytes.Contains(ts.screen(), []byte(">2")) || bytes.Contains(ts.screen(), []byte(">1")) {
		t.Error("a vendor without quota was preselected")
	}
	ts.typeIn(t, "\x1b")
	finish(t, c, done)
}

// TestNearLimitStatusFits (T6699, L60-4): the warning fits the width,
// ASCII only, never the last column; it keeps vendor and share down to
// 40 columns and its keys down to 13.
func TestNearLimitStatusFits(t *testing.T) {
	key := keyName(0x1c)
	for _, v := range adapter.Vendors() {
		for _, to := range append(adapter.Vendors(), "") {
			if to == v {
				continue // the picker never offers the vendor on screen
			}
			p := &picker{key: 0x1c}
			p.warn(to, warnForms(v, 100, key, to)...)
			for _, cols := range []int{80, 60, 50, 45, 40, 30, 20, 13} {
				line := p.statusLine(cols)
				if len(line) > cols-1 {
					t.Errorf("%s->%q at %d columns: %d bytes: %q", v, to, cols, len(line), line)
				}
				for _, r := range line {
					if r > 0x7e || r < 0x20 {
						t.Errorf("non-ASCII or control %q in %q", r, line)
					}
				}
				if !strings.HasSuffix(line, "Esc") && !strings.HasSuffix(line, "Esc dismisses.") {
					t.Errorf("%s->%q at %d columns: Esc is cut: %q", v, to, cols, line)
				}
				if to != "" && !strings.Contains(line, key) {
					t.Errorf("%s->%q at %d columns: %s is cut: %q", v, to, cols, key, line)
				}
				if cols >= 40 && !strings.Contains(line, string(v)) {
					t.Errorf("%s->%q at %d columns: the vendor is cut: %q", v, to, cols, line)
				}
				if cols >= 40 && !strings.Contains(line, "100%") {
					t.Errorf("%s->%q at %d columns: the share is cut: %q", v, to, cols, line)
				}
				if cols == 80 && !strings.HasPrefix(line, "aggy: "+string(v)+" is at 100% of its limit.") {
					t.Errorf("80 columns: not the full line: %q", line)
				}
			}
		}
	}
}
