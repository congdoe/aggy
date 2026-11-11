//go:build unix

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/trustlist"
)

// trustAnswer is the one folder answer in aggy's trust file, and whether
// there is one.
func trustAnswer(t *testing.T, w *world) (trustlist.Folder, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(w.home, ".aggy", "trust.json"))
	if os.IsNotExist(err) {
		return trustlist.Folder{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var f trustlist.File
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	for _, a := range f.Folders {
		return a, true
	}
	return trustlist.Folder{}, false
}

// keys sends each key as its own read, as a person types: the list drops a
// read of several keys, and the typed list drops a line within 150 ms of
// asking again.
func (s *session) keys(ks ...string) {
	s.t.Helper()
	for _, k := range ks {
		s.send([]byte(k))
		time.Sleep(400 * time.Millisecond)
	}
}

// TestTrustSelectFirstStart (T6981): the first start's trust list is a
// select drawn in place before Claude starts: past the typed-ahead guard, Up
// to All, Space, and Enter tick the harnesses and save the answer; a typed
// number still ticks its row; TERM=dumb gets the typed list. Claude then
// starts, and the terminal is put back after.
func TestTrustSelectFirstStart(t *testing.T) {
	cases := []struct {
		name  string
		env   []string
		ready string // what the list shows once it is up
		keys  []string
	}{
		{"arrows, Space, Enter", nil, trustlist.SelectKeys[:20], []string{"\x1b[A", " ", "\r"}},
		{"a typed number", nil, trustlist.SelectKeys[:20], []string{"2", "\r"}},
		{"the typed list on a dumb terminal", []string{"TERM=dumb"}, "Type numbers", []string{"2\r", "\r"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, sc, _ := commandWorld(t)
			w.askTrust, w.env = true, c.env
			s := w.start(t, sc, "run", "claude")
			s.waitWithin([]byte(c.ready), offerTimeout)
			time.Sleep(300 * time.Millisecond) // past the typed-ahead guard
			s.keys(c.keys...)
			s.waitWithin([]byte("Saved this folder's answer"), offerTimeout)
			s.waitWithin([]byte("READY>"), offerTimeout)
			s.send([]byte("/exit\r"))
			if code := s.exit(); code != 0 {
				t.Fatalf("aggy exit = %d\n%q", code, s.screen())
			}
			s.restored("after the list")
			if a, ok := trustAnswer(t, w); !ok || !slices.Equal(a.On, []string{"claude"}) {
				t.Errorf("answer %+v (%v)", a, ok)
			}
		})
	}
}

// TestTrustSelectEnds (T6981): Esc is not now and Claude starts; Ctrl-C and
// SIGTERM end aggy at the list. Every way out puts the terminal back, and
// none records an answer. An Enter typed ahead of the list confirms nothing.
func TestTrustSelectEnds(t *testing.T) {
	cases := []struct {
		name   string
		early  string // typed as the list appears
		key    string
		sig    syscall.Signal
		starts bool // Claude starts after
	}{
		{"Esc", "", "\x1b", 0, true},
		{"an Enter typed ahead, then Esc", "\r", "\x1b", 0, true},
		{"Ctrl-C", "", "\x03", syscall.SIGINT, false},
		{"SIGTERM", "", "", syscall.SIGTERM, false},
		{"SIGHUP", "", "", syscall.SIGHUP, false},
		{"SIGQUIT", "", "", syscall.SIGQUIT, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, sc, _ := commandWorld(t)
			w.askTrust = true
			s := w.start(t, sc, "run", "claude")
			s.waitWithin([]byte(trustlist.SelectKeys[:20]), offerTimeout)
			wait := 300 * time.Millisecond // past the typed-ahead guard
			if c.early != "" {
				s.send([]byte(c.early))
				// A key typed ahead starts the typing guard: keys count
				// again after a second's pause.
				wait = 1500 * time.Millisecond
			}
			time.Sleep(wait)
			switch {
			case c.key != "":
				s.keys(c.key)
			default:
				if err := s.cmd.Process.Signal(c.sig); err != nil {
					t.Fatal(err)
				}
			}
			if c.starts {
				s.waitWithin([]byte("Not now"), offerTimeout)
				s.waitWithin([]byte("READY>"), offerTimeout)
				s.send([]byte("/exit\r"))
			}
			code := s.exit()
			ws, _ := s.cmd.ProcessState.Sys().(syscall.WaitStatus)
			// Go's own default for SIGQUIT is a goroutine dump and exit 2.
			died := ws.Signaled() && ws.Signal() == c.sig || code == 128+int(c.sig) || c.sig == syscall.SIGQUIT && code == 2
			if c.sig != 0 && !died {
				t.Errorf("aggy exit = %d (%v), want death by %v", code, ws, c.sig)
			}
			if c.sig == 0 && code != 0 {
				t.Errorf("aggy exit = %d", code)
			}
			s.restored("after the list")
			if a, ok := trustAnswer(t, w); ok {
				t.Errorf("an answer was recorded: %+v", a)
			}
		})
	}
}
