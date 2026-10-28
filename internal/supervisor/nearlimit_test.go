//go:build unix

package supervisor

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// usedOpts reports *share (in thousandths; negative: no data) as the
// share of the on-screen vendor's limit used, warns at warnAt, and gives
// grok quota.
func usedOpts(share *atomic.Int64, warnAt float64) func(*Options) {
	return func(o *Options) {
		o.WarnAt = warnAt
		o.Used = func(adapter.Vendor) (float64, bool) {
			n := share.Load()
			return float64(n) / 1000, n >= 0
		}
		o.HasQuota = func(v adapter.Vendor) bool { return v == adapter.Grok }
	}
}

const codexStop = `{"type":"hook-event","vendor":"codex","event":"Stop","payload":{"session_id":"` + childSID + `"}}`

// TestNearLimitWarn (T6699): a turn end of the on-screen child at or past
// warn_at shows one status line, once per crossing; below it, or for
// another session's turn end, nothing. The switch key opens the list with
// the first vendor with quota preselected; nothing switches by itself, and
// unsent text still blocks the switch.
func TestNearLimitWarn(t *testing.T) {
	var share atomic.Int64
	share.Store(500)
	ts, c, done, sock := switchRunOn(t, false, adapter.Codex, usedOpts(&share, 0.9))
	const want = "aggy: codex is at 90% of its limit. Ctrl+\\ hands off to grok, Esc dismisses."

	_ = request(t, sock, codexStop) // below
	never(t, "a warning below warn_at", []byte("of its limit"), ts.screen, 200*time.Millisecond)

	share.Store(900) // exactly at warn_at
	_ = request(t, sock, codexStop)
	poll(t, "the warning", []byte("\x1b7\x1b[24;1H\x1b[2K\x1b[7m"+want+"\x1b[0m\x1b8"), ts.screen)
	ts.typeIn(t, "\x1b")
	poll(t, "the line cleared", []byte("\x1b7\x1b[24;1H\x1b[2K\x1b8"), ts.screen)

	share.Store(950) // still past: once per crossing
	mark := len(ts.screen())
	_ = request(t, sock, codexStop)
	never(t, "a second warning", []byte("of its limit"), func() []byte { return ts.screen()[mark:] }, 200*time.Millisecond)

	share.Store(300) // a reset: the next crossing warns again
	_ = request(t, sock, codexStop)
	time.Sleep(150 * time.Millisecond)
	share.Store(910)
	_ = request(t, sock, `{"type":"hook-event","vendor":"codex","event":"Stop","payload":{"session_id":"aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}}`)
	never(t, "a nested session's turn end", []byte("of its limit"), func() []byte { return ts.screen()[mark:] }, 200*time.Millisecond)
	_ = request(t, sock, codexStop)
	poll(t, "the warning again", []byte("codex is at 91% of its limit"), func() []byte { return ts.screen()[mark:] })

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

// TestNearLimitSilent (T6699): no warning without data (a vendor that
// writes no allowance locally), with warn_at 0, or with no Used at all.
func TestNearLimitSilent(t *testing.T) {
	for _, tt := range []struct {
		name   string
		share  int64
		warnAt float64
	}{
		{"no local data", -1, 0.9},
		{"turned off", 990, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var share atomic.Int64
			share.Store(tt.share)
			ts, c, done, sock := switchRunOn(t, false, adapter.Claude, usedOpts(&share, tt.warnAt))
			_ = request(t, sock, stopLine)
			never(t, "a warning", []byte("of its limit"), ts.screen, 300*time.Millisecond)
			finish(t, c, done)
		})
	}
}

// TestNearLimitNoQuota (T6699): with no other vendor left the warning
// says so; the switch key still opens the list, with nothing preselected.
func TestNearLimitNoQuota(t *testing.T) {
	var share atomic.Int64
	share.Store(990)
	ts, c, done, sock := switchRunOn(t, false, adapter.Codex, usedOpts(&share, 0.9), func(o *Options) {
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

// TestNearLimitStatusFits (T6699): the warning fits the width, ASCII
// only, never the last column, and the keys stay in view as it narrows.
func TestNearLimitStatusFits(t *testing.T) {
	for _, v := range adapter.Vendors() {
		for _, to := range adapter.Vendors() {
			if to == v {
				continue // the picker never offers the vendor on screen
			}
			p := &picker{key: 0x1c}
			p.warn(to, string(v)+" is at 100% of its limit.", " "+keyName(0x1c)+" hands off to "+string(to)+", Esc dismisses.")
			for _, cols := range []int{80, 60, 40, 20} {
				line := p.statusLine(cols)
				if len(line) > cols-1 {
					t.Errorf("%s->%s at %d columns: %d bytes: %q", v, to, cols, len(line), line)
				}
				for _, r := range line {
					if r > 0x7e || r < 0x20 {
						t.Errorf("non-ASCII or control %q in %q", r, line)
					}
				}
				if cols >= 60 && !strings.HasSuffix(line, "Esc dismisses.") {
					t.Errorf("%d columns: the keys are cut: %q", cols, line)
				}
				if cols == 80 && !strings.HasPrefix(line, "aggy: "+string(v)+" is at 100% of its limit.") {
					t.Errorf("80 columns: the line is cut: %q", line)
				}
			}
		}
	}
}
