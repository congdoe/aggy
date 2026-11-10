//go:build unix

package supervisor

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/trustlist"
)

// trustSess is viewSess with a trust list of two harnesses and a source,
// whose answers are recorded.
func trustSess(t *testing.T) (*session, *[]string) {
	t.Helper()
	s, _ := viewSess(t, adapter.Claude, "")
	old := checkTypeAhead
	checkTypeAhead = 0
	t.Cleanup(func() { checkTypeAhead = old })
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
		{"Down and Space", []string{"\x1b[B", " ", "\r"}, []string{"claude confirm"}, "Saved."},
		{"a paste picks nothing", []string{"\x1b[200~1\r\x1b[201~", "\r"}, []string{" confirm"}, "Saved."},
		{"never", []string{"2", "x"}, []string{"claude never"}, "Saved."},
		{"not now", []string{"1", "n"}, nil, ""},
		{"Esc", []string{"1", "\x1b"}, nil, ""},
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
			for _, w := range []string{"Trust /repo?", "All harnesses", "Claude Code", "! plugins run", "Enter confirms"} {
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
	if len(*answers) != 0 || s.pk.menu == nil || len(s.pk.menu.check.st.Ticked()) != 0 {
		t.Fatalf("typed-ahead keys answered: %q", *answers)
	}
}

// TestTrustListKeyboardOnly: no hook opens the list, and neither does a
// session with no list to read.
func TestTrustListKeyboardOnly(t *testing.T) {
	s, _ := trustSess(t)
	if err := s.onPromptCommand(Request{Vendor: "claude", Command: "/aggy trust"}); err == nil || !strings.Contains(err.Error(), "only typed in full") {
		t.Errorf("a hook's /aggy trust: %v", err)
	}
	s.o.TrustList = nil
	typeLine(s, "/aggy trust")
	if s.open {
		t.Error("a session with no list opened one")
	}
}
