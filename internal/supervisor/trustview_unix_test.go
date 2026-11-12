//go:build unix

package supervisor

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/trustlist"
)

// trustSess is viewSess with a trust list of two harnesses and a source,
// whose answers are recorded.
func trustSess(t *testing.T) (*session, *[]string) {
	t.Helper()
	s, _ := viewSess(t, adapter.Claude, "")
	old, oldNow := checkTypeAhead, checkNow
	checkTypeAhead = 0
	// Keys a second apart, as a person answering presses them, unless a
	// test sets checkGap.
	checkGap = time.Second
	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	checkNow = func() time.Time { at = at.Add(checkGap); return at }
	t.Cleanup(func() { checkTypeAhead, checkNow = old, oldNow })
	var answers []string
	s.o.TrustList = func() (TrustList, error) {
		st := trustlist.NewState([]trustlist.Row{
			{ID: "claude", Label: "Claude Code", Group: trustlist.Harness},
			{ID: "opencode", Label: "OpenCode", Group: trustlist.Harness, Warning: "plugins run"},
			{ID: "opencode-free", Label: "OpenCode free models", Group: trustlist.Source},
		}, trustlist.Folder{}, false, nil)
		return TrustList{Lines: []string{"Trust /repo?"}, State: st}, nil
	}
	s.o.TrustAnswer = func(ticked []string, c trustlist.Choice) string {
		answers = append(answers, strings.Join(ticked, ",")+map[trustlist.Choice]string{trustlist.Confirm: " confirm", trustlist.Never: " never"}[c])
		return "Saved."
	}
	return s, &answers
}

// TestTrustListView (T6977): /aggy trust opens the folder's list on aggy's
// screen; keys tick rows (All the harnesses only), a paste picks nothing, and
// Enter confirms, x says never, n or Esc records nothing.
func TestTrustListView(t *testing.T) {
	cases := []struct {
		name  string
		keys  []string
		want  []string // the answers given
		after string   // shown after, if anything was answered
	}{
		{"All and Enter", []string{"1", "\r"}, []string{"claude,opencode confirm"}, "Saved."},
		{"a source is its own tick", []string{"1", "4", "\r"}, []string{"claude,opencode,opencode-free confirm"}, "Saved."},
		// T6981: the cursor starts on Claude Code, the first row with no
		// warning, not on All.
		{"Enter at once grants nothing", []string{"\r"}, []string{" confirm"}, "Saved."},
		{"Space at once", []string{" ", "\r"}, []string{"claude confirm"}, "Saved."},
		{"Down and Space", []string{"\x1b[B", " ", "\r"}, []string{"opencode confirm"}, "Saved."},
		{"Up to All and Space", []string{"\x1b[A", " ", "\r"}, []string{"claude,opencode confirm"}, "Saved."},
		{"Ctrl-N, Ctrl-P, j, k", []string{"\x0e", "\x0e", "\x10", "j", "k", " ", "\r"}, []string{"opencode confirm"}, "Saved."},
		{"a paste picks nothing", []string{"\x1b[200~1\r\x1b[201~", "\r"}, []string{" confirm"}, "Saved."},
		// A burst or an unknown sequence is typing: an Enter right after it
		// confirms nothing; a move after a pause, then Enter, does.
		{"a burst picks nothing", []string{"2\r", "x\r", "\r", "\x1b[B", "\r"}, []string{" confirm"}, "Saved."},
		{"an unknown sequence is not Esc then x", []string{"\x1bx", "\x1b[1;5A", "\r", "\x1b[B", "\r"}, []string{" confirm"}, "Saved."},
		{"never needs Enter after x", []string{"2", "x", "\r"}, []string{"claude never"}, "Saved."},
		{"Esc while typing still closes", []string{"1", "a", "\x1b"}, nil, ""},
		{"not now", []string{"1", "n"}, nil, ""},
		{"Esc", []string{"1", "\x1b"}, nil, ""},
		{"Ctrl-C", []string{"1", "\x03"}, nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, answers := trustSess(t)
			typeLine(s, "/aggy trust")
			settle(t, s)
			if s.pk.menu == nil || s.pk.menu.check == nil {
				t.Fatalf("no list: menu %+v page %+v", s.pk.menu, s.pk.page)
			}
			scr := screenOf(t, s)
			for _, w := range []string{"Trust /repo?", "All harnesses", ">  2 [ ] Claude Code", "! plugins run", "Up/Down move; Space ticks; Enter confirms"} {
				if !strings.Contains(scr, w) {
					t.Errorf("the list does not show %q", w)
				}
			}
			for _, k := range c.keys {
				s.viewInput([]byte(k))
			}
			if c.after != "" {
				settle(t, s)
			}
			if !slices.Equal(*answers, c.want) {
				t.Errorf("answers %q, want %q", *answers, c.want)
			}
			if c.after == "" && s.open {
				t.Error("the list stayed up")
			}
			if c.after != "" && (s.pk.page == nil || !slices.Contains(s.pk.page.lines, c.after)) {
				t.Errorf("page %+v", s.pk.page)
			}
		})
	}
}

// TestTrustListTypedAhead: keys that come before the list has been up for
// checkTypeAhead were typed before it, and answer nothing.
func TestTrustListTypedAhead(t *testing.T) {
	s, answers := trustSess(t)
	checkTypeAhead = time.Hour
	typeLine(s, "/aggy trust")
	settle(t, s)
	s.viewInput([]byte("1"))
	s.viewInput([]byte("\r"))
	if len(*answers) != 0 || s.pk.menu == nil || len(s.pk.menu.check.sel.St.Ticked()) != 0 {
		t.Fatalf("typed-ahead keys answered: %q", *answers)
	}
}

// checkGap is how far apart trustSess's clock puts the keys.
var checkGap time.Duration

// TestTrustListTyping (PR #140 review, H1): text typed at the page a byte a
// read, at typing speed, answers nothing and leaves the list up, with nothing
// ticked; the page says it is ignoring typed text. Each fails at 4de725e.
func TestTrustListTyping(t *testing.T) {
	for _, text := range []string{"ok go\r", "see issue 4\r", "add tests\r", "fix it\r", "just do it\r", " hello\r"} {
		t.Run(strings.TrimSpace(text), func(t *testing.T) {
			s, answers := trustSess(t)
			typeLine(s, "/aggy trust")
			settle(t, s)
			checkGap = 80 * time.Millisecond
			for _, b := range []byte(text) {
				s.viewInput([]byte{b})
			}
			if len(*answers) != 0 || !s.open || s.pk.menu == nil || len(s.pk.menu.check.sel.St.Ticked()) != 0 {
				t.Fatalf("typing answered: %q", *answers)
			}
			if !strings.Contains(screenOf(t, s), "Ignoring typed text") {
				t.Error("the page does not say it ignores typed text")
			}
			// PR #140 re-review, M2: Enter after a pause is ignored, and the
			// page says why.
			checkGap = 2 * time.Second
			s.viewInput([]byte("\r"))
			if len(*answers) != 0 || !s.open {
				t.Fatalf("Enter after typing answered: %q", *answers)
			}
			if scr := screenOf(t, s); !strings.Contains(scr[strings.LastIndex(scr, "Trust /repo?"):], trustlist.EnterNote) {
				t.Error("the page does not say why Enter did nothing")
			}
		})
	}
}

// TestTrustListKeyboardOnly: no hook opens the list, and neither does a
// session with no list to read.
func TestTrustListKeyboardOnly(t *testing.T) {
	s, _ := trustSess(t)
	err := hookAsk(s, Request{Type: ReqPromptCommand, Vendor: "claude", Command: "/aggy trust"})
	var be *blockedErr
	if codeOf(err, diag.Code{}) != diag.CodeCommandKeyboard || !errors.As(err, &be) || s.open {
		t.Errorf("a hook's /aggy trust: %v, open %v", err, s.open)
	}
	s.o.TrustList = nil
	typeLine(s, "/aggy trust")
	if s.open {
		t.Error("a session with no list opened one")
	}
}
