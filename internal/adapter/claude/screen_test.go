package claude

import (
	"encoding/json"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/screen"
)

// screenFixtures is the recording of Claude Code 2.1.326's screen
// (docs/verification.md, "Claude Code's screen").
const screenFixtures = "../../../testdata/fixtures/claude/screen"

type screenMarksFile struct {
	Version    string
	Rows, Cols int
	Marks      []struct {
		State    string
		From, To int
	}
}

func readScreenFixture(t *testing.T) ([]byte, screenMarksFile) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(screenFixtures, "claude.raw"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(screenFixtures, "claude.marks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m screenMarksFile
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return raw, m
}

// chunkings feed b to s whole, one byte at a time, or in random sizes.
var chunkings = []struct {
	name string
	feed func(s *screen.Screen, b []byte, rng *rand.Rand)
}{
	{"whole", func(s *screen.Screen, b []byte, _ *rand.Rand) { s.Feed(b) }},
	{"bytes", func(s *screen.Screen, b []byte, _ *rand.Rand) {
		for i := range b {
			s.Feed(b[i : i+1])
		}
	}},
	{"random", func(s *screen.Screen, b []byte, rng *rand.Rand) {
		for len(b) > 0 {
			n := min(1+rng.IntN(64), len(b))
			s.Feed(b[:n])
			b = b[n:]
		}
	}},
}

// TestClassifyRecording (T7016): the live recording replayed mark by mark,
// whole and in 1-byte and random-size chunks; after each mark the class is
// what the screen showed. slash_menu may be PromptText or Other, and
// bash_mode anything but PromptEmpty; exit is not judged.
func TestClassifyRecording(t *testing.T) {
	raw, m := readScreenFixture(t)
	if m.Version != screenMarks.version {
		t.Fatalf("the recording is of %s, the marks of %s", m.Version, screenMarks.version)
	}
	empty, text, busy, other := adapter.ScreenPromptEmpty, adapter.ScreenPromptText, adapter.ScreenBusy, adapter.ScreenOther
	want := map[string][]adapter.ScreenState{
		"start_idle":       {empty},
		"typed_text":       {text},
		"cleared":          {empty},
		"slash_menu":       {text, other},
		"cleared_menu":     {empty},
		"bash_mode":        {text, other, busy},
		"bash_mode_left":   {empty},
		"model_dialog":     {other},
		"model_dialog_esc": {empty},
		"left_view":        {empty}, // Left drew nothing at 2.1.326
		"left_view_esc":    {empty},
		"idle_again":       {empty},
		"turn_running":     {busy},
		"turn_done":        {empty},
	}
	for _, ch := range chunkings {
		t.Run(ch.name, func(t *testing.T) {
			rng := rand.New(rand.NewPCG(7016, 1))
			s := screen.New(m.Rows, m.Cols)
			for _, k := range m.Marks {
				ch.feed(s, raw[k.From:k.To], rng)
				got := ClassifyScreen(m.Version, s.Snapshot())
				w, ok := want[k.State]
				if !ok {
					continue
				}
				found := false
				for _, x := range w {
					found = found || x == got
				}
				if !found {
					t.Errorf("after %s: %v, want one of %v", k.State, got, w)
				}
			}
		})
	}
}

// TestClassifyTrustDialog (T7016): Claude 2.1.326 in a folder it had not
// seen, its folder-trust dialog ("❯ No, exit" selected), then Esc and its
// exit text: never the prompt, at any byte.
func TestClassifyTrustDialog(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(screenFixtures, "claude-newfolder.raw"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "❯\x1b[4GNo,") {
		t.Fatal("the recording no longer holds the dialog's first option")
	}
	for _, cols := range []int{100, 80} {
		s := screen.New(30, cols)
		for i := range raw {
			s.Feed(raw[i : i+1])
			if got := ClassifyScreen("2.1.326", s.Snapshot()); got != adapter.ScreenOther {
				t.Fatalf("at %d columns, after byte %d: %v", cols, i, got)
			}
		}
	}
}

// TestClassifyMarks (T7016): each mark is needed. The recording's idle
// screen reads as the empty prompt; take any one mark away and it does not.
func TestClassifyMarks(t *testing.T) {
	raw, m := readScreenFixture(t)
	idle := raw[m.Marks[0].From:m.Marks[0].To]
	for _, c := range []struct {
		name, version string
		after         string
		rows, cols    int
		want          adapter.ScreenState
	}{
		{"the recording", "2.1.326", "", 30, 100, adapter.ScreenPromptEmpty},
		{"a later patch", "2.1.340", "", 30, 100, adapter.ScreenPromptEmpty},
		{"an earlier patch", "2.1.322", "", 30, 100, adapter.ScreenOther},
		{"another minor", "2.2.0", "", 30, 100, adapter.ScreenOther},
		{"no version", "", "", 30, 100, adapter.ScreenOther},
		{"a pre-release", "2.1.330-beta", "", 30, 100, adapter.ScreenOther},
		{"the cursor hidden", "2.1.326", "\x1b[?25l", 30, 100, adapter.ScreenOther},
		{"the cursor one column right", "2.1.326", "\x1b[C", 30, 100, adapter.ScreenOther},
		{"the cursor on the rule", "2.1.326", "\x1b[A", 30, 100, adapter.ScreenOther},
		{"the cursor below", "2.1.326", "\x1b[B", 30, 100, adapter.ScreenOther},
		{"text typed", "2.1.326", "x", 30, 100, adapter.ScreenPromptText},
		{"faint text typed", "2.1.326", "\x1b[2mx\x1b[22m\x1b[D", 30, 100, adapter.ScreenPromptEmpty},
		{"text after the placeholder", "2.1.326", "\x1b[60Gx\x1b[3G", 30, 100, adapter.ScreenPromptText},
		{"no no-break space", "2.1.326", "\r❯ \x1b[3G", 30, 100, adapter.ScreenOther},
		{"no prompt mark", "2.1.326", "\r>\x1b[3G", 30, 100, adapter.ScreenOther},
		{"the upper rule cut", "2.1.326", "\x1b[A\x1b[50G \x1b[B\x1b[3G", 30, 100, adapter.ScreenOther},
		{"the lower rule cut", "2.1.326", "\x1b[B\x1b[100G \x1b[A\x1b[3G", 30, 100, adapter.ScreenOther},
		{"a menu below", "2.1.326", "\x1b[3B\x1b[3G❯ /x\x1b[3A\x1b[3G", 30, 100, adapter.ScreenOther},
		{"a turn running", "2.1.326", "\x1b[3B\x1b[40Gesc to interrupt\x1b[3A\x1b[3G", 30, 100, adapter.ScreenBusy},
		{"the alternate screen", "2.1.326", "\x1b[?1049h", 30, 100, adapter.ScreenOther},
		{"back from the alternate screen", "2.1.326", "\x1b[?1049h\x1b[2J\x1b[?1049l", 30, 100, adapter.ScreenPromptEmpty},
		{"unsure: a scroll region", "2.1.326", "\x1b7\x1b[2;10r\x1b8", 30, 100, adapter.ScreenOther},
		{"unsure: an absolute row", "2.1.326", "\x1b7\x1b[5;1Hx\x1b8", 30, 100, adapter.ScreenOther},
		{"a split sequence", "2.1.326", "\x1b[", 30, 100, adapter.ScreenOther},
		{"a narrower screen", "2.1.326", "", 30, 99, adapter.ScreenOther},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := screen.New(c.rows, c.cols)
			s.Feed(idle)
			s.Feed([]byte(c.after))
			if got := ClassifyScreen(c.version, s.Snapshot()); got != c.want {
				t.Errorf("%v, want %v", got, c.want)
			}
		})
	}
	if got := ClassifyScreen("2.1.326", nil); got != adapter.ScreenOther {
		t.Errorf("no screen: %v", got)
	}
}
