//go:build unix

package supervisor

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// viewSess is cmdSess whose aggy commands are recorded and answered with out.
func viewSess(t *testing.T, v adapter.Vendor, out string) (*session, *[][]string) {
	t.Helper()
	s := cmdSess(t, v)
	s.cmdResults = make(chan cmdResult, 4)
	var ran [][]string
	s.o.Aggy = func(args []string, answer bool) string {
		ran = append(ran, append(append([]string(nil), args...), fmt.Sprint(answer)))
		return out
	}
	s.o.ToolNames = func() []string { return []string{"rtk", "caveman"} }
	s.o.Tools = func() (ToolSet, error) { return ToolSet{"rtk": "mcp"}, nil }
	return s, &ran
}

// typeLine types a command one key per read, then its Enter alone.
func typeLine(s *session, line string) {
	for _, k := range strings.Split(line, "") {
		s.send([]byte(k))
	}
	s.send([]byte("\r"))
}

// settle hands the session the run's result, as the main loop would.
func settle(t *testing.T, s *session) {
	t.Helper()
	select {
	case r := <-s.cmdResults:
		s.cmdDone(r)
	case <-time.After(waitLimit):
		t.Fatal("the run gave no result")
	}
}

func screenOf(t *testing.T, s *session) string {
	t.Helper()
	b, err := os.ReadFile(s.o.Out.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestCommandPage (T6909): a command with no turn shows its output on the
// alternate screen, and Esc returns; nothing of the line reaches the CLI but
// its keys and Ctrl+U.
func TestCommandPage(t *testing.T) {
	for _, c := range []struct {
		on   adapter.Vendor
		line string
		want []string
	}{
		{adapter.Claude, "/inbox", []string{"inbox"}},
		{adapter.Claude, "/aggy usage", []string{"usage"}},
		{adapter.Codex, "$aggy stats", []string{"stats"}},
		{adapter.Claude, "/aggy log", []string{"log", "-n", "200"}},
		{adapter.Claude, "/aggy brief", []string{"brief", "--show", "--from", "claude", "--session", childSID}},
	} {
		t.Run(c.line, func(t *testing.T) {
			s, ran := viewSess(t, c.on, "line one\n\x1b[31mred\x1b[0m\n")
			typeLine(s, c.line)
			if s.pk.page == nil || !s.open {
				t.Fatal("no page opened")
			}
			settle(t, s)
			if len(*ran) != 1 || strings.Join((*ran)[0], " ") != strings.Join(c.want, " ")+" false" {
				t.Fatalf("ran %q, want %q", *ran, c.want)
			}
			scr := screenOf(t, s)
			if !strings.Contains(scr, altScreenOn) || !strings.Contains(scr, "  line one") || !strings.Contains(scr, "  red") || strings.Contains(scr, "\x1b[31m") {
				t.Fatalf("screen %q", scr)
			}
			s.viewInput([]byte("\x1b"))
			if s.open || s.pk.page != nil {
				t.Fatal("Esc left the page up")
			}
			if got := sentTo(s); got != c.line+"\x15" {
				t.Errorf("the CLI got %q", got)
			}
		})
	}
}

// TestCommandMenus (T6909): a command with its arguments left out lists its
// options; a number or Enter picks one, which runs or fills in the line, and a
// paste picks nothing.
func TestCommandMenus(t *testing.T) {
	t.Run("aggy lists every command", func(t *testing.T) {
		s, ran := viewSess(t, adapter.Claude, "ok")
		typeLine(s, "/aggy")
		if s.pk.menu == nil {
			t.Fatal("no menu")
		}
		scr := screenOf(t, s)
		for _, w := range []string{"/switch", "/borrow", "/inbox", "aggy usage", "aggy accept", "aggy tool", "aggy trust"} {
			if !strings.Contains(scr, w) {
				t.Errorf("the list lacks %q: %q", w, scr)
			}
		}
		s.viewInput([]byte("\x1b[200~3\x1b[201~"))
		s.viewInput([]byte("\x1b[200"))
		s.viewInput([]byte("~1\r\x1b[201~"))
		if s.pk.menu == nil || len(*ran) > 0 {
			t.Fatal("a paste picked from the list")
		}
		s.viewInput([]byte("3")) // /inbox
		settle(t, s)
		if len(*ran) != 1 || (*ran)[0][0] != "inbox" {
			t.Fatalf("ran %q", *ran)
		}
	})
	t.Run("borrow fills in the line", func(t *testing.T) {
		s, _ := viewSess(t, adapter.Codex, "")
		typeLine(s, "$borrow")
		s.viewInput([]byte("\x1b[B\x1b[B\r")) // research
		if s.open {
			t.Fatal("the list stayed up")
		}
		if got := sentTo(s); got != "$borrow\x15$borrow research " {
			t.Fatalf("the CLI got %q", got)
		}
	})
	t.Run("tool lists the tools", func(t *testing.T) {
		s, ran := viewSess(t, adapter.Claude, "ok")
		typeLine(s, "/aggy tool")
		settle(t, s)
		if s.pk.menu == nil || len(s.pk.menu.items) != 2 {
			t.Fatalf("menu %+v", s.pk.menu)
		}
		s.viewInput([]byte("1")) // rtk is on: picking turns it off
		settle(t, s)
		if len(*ran) != 1 || strings.Join((*ran)[0], " ") != "tool off rtk false" {
			t.Fatalf("ran %q", *ran)
		}
	})
}

// TestCommandConfirm (T6909): a command that changes something shows what it
// would do first, and only a typed y runs it.
func TestCommandConfirm(t *testing.T) {
	s, ran := viewSess(t, adapter.Claude, "Not applied: run again with --yes")
	typeLine(s, "/aggy accept 9f2")
	settle(t, s)
	s.viewInput([]byte("\x1b[200~y\x1b[201~n"))
	if len(*ran) != 1 {
		t.Fatalf("ran %q before y", *ran)
	}
	s.viewInput([]byte("y"))
	settle(t, s)
	if len(*ran) != 2 || strings.Join((*ran)[1], " ") != "accept 9f2 --yes false" {
		t.Fatalf("ran %q", *ran)
	}
	s.viewInput([]byte("y"))
	if len(*ran) != 2 {
		t.Fatal("y ran it twice")
	}
	s2, ran2 := viewSess(t, adapter.Claude, "Trust it? [y/N] Not trusted.")
	typeLine(s2, "/aggy trust --opencode")
	settle(t, s2)
	s2.viewInput([]byte("y"))
	settle(t, s2)
	if len(*ran2) != 2 || strings.Join((*ran2)[1], " ") != "trust --opencode . true" {
		t.Fatalf("ran %q", *ran2)
	}
}

// TestPlainText: escape sequences and control characters never reach the screen.
func TestPlainText(t *testing.T) {
	got := strings.Join(plainText("a\x1b[2Jb\tc\x07\r\nd\x1b]0;x\x07e\u009bf\n"), "|")
	if strings.ContainsAny(got, "\x1b\x07\r\u009b") || !strings.HasPrefix(got, "ab    c|d") {
		t.Errorf("plainText = %q", got)
	}
}

// TestPasteFilter: pastes are dropped, markers split across reads included.
func TestPasteFilter(t *testing.T) {
	for _, c := range []struct {
		reads []string
		want  string
	}{
		{[]string{"a\x1b[200~xyz\x1b[201~b"}, "ab"},
		{[]string{"a\x1b[2", "00~x", "y\x1b[20", "1~b"}, "ab"},
		{[]string{"\x1b[A", "1"}, "\x1b[A1"},
		{[]string{"\x1b"}, "\x1b"},
	} {
		var f pasteFilter
		var got []byte
		for _, r := range c.reads {
			got = append(got, f.drop([]byte(r))...)
		}
		if string(got) != c.want {
			t.Errorf("%q: got %q, want %q", c.reads, got, c.want)
		}
	}
}
