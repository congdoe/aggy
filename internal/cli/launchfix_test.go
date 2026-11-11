package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/store"
)

// fixWorld is a home with no hooks and no skills linked, this binary as aggy,
// and a terminal that answers typed.
func fixWorld(t *testing.T, typed string, term bool) (string, *store.Store) {
	t.Helper()
	const bin = "/opt/homebrew/bin/aggy"
	h := isolate(t)
	oldBin, oldPath, oldCheck, oldAsked := hookBin, pathAggy, checkTrusted, setupAskedNow
	hookBin = func() (string, error) { return bin, nil }
	pathAggy = func() (string, error) { return bin, nil }
	checkTrusted = func(string) error { return nil }
	t.Cleanup(func() { hookBin, pathAggy, checkTrusted, setupAskedNow = oldBin, oldPath, oldCheck, oldAsked })
	for _, d := range []string{".claude", ".codex"} {
		if err := os.MkdirAll(filepath.Join(h, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	inPlaceWorld(t, typed, term, false)
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return h, s
}

// TestOfferLaunchFix (T6982): a launch that finds aggy's hooks or command
// skills missing for the CLI asks once whether to add them, naming every file
// a yes changes; a yes writes both in one manifest run; a no is kept for this
// aggy version and also silences the skills' line; nobody to ask, or a person
// asked about setup in this same process, gets the lines only.
func TestOfferLaunchFix(t *testing.T) {
	for _, c := range []struct {
		name     string
		typed    string
		term     bool
		declined string // the note before
		setup    bool   // the first-run question was asked in this process
		asked    bool
		fixed    bool
		note     string // the note after
	}{
		{"yes", "y\n", true, "", false, true, true, ""},
		{"no", "n\n", true, "", false, true, false, "v1"},
		{"no terminal", "y\n", false, "", false, false, false, ""},
		{"no given for this version", "y\n", true, "v1", false, false, false, "v1"},
		{"no given for another version", "y\n", true, "v0", false, true, true, "v0"},
		{"setup asked already", "y\n", true, "", true, false, false, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			h, s := fixWorld(t, c.typed, c.term)
			ctx := context.Background()
			if c.declined != "" {
				if err := s.SetNote(ctx, fixDeclinedNote, c.declined, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			setupAskedNow = c.setup
			declined := declinedFor(ctx, s, "v1")
			f := findLaunchFix(ctx, adapter.Claude, false, declined)
			if f.hookLine == "" || len(f.hooks) == 0 || !f.planned {
				t.Fatalf("found %+v", f)
			}
			if (f.skillLine == "") != declined {
				t.Fatalf("skill line %q with a no for this version %v", f.skillLine, declined)
			}
			var out bytes.Buffer
			left := offerLaunchFix(ctx, s, adapter.Claude, f, "v1", os.Getenv, offerIn, &out)
			if got := strings.Contains(out.String(), "[y/N]"); got != c.asked {
				t.Errorf("asked %v, want %v: %q", got, c.asked, out.String())
			}
			if c.asked {
				// #142 review: the question names what one y changes.
				for _, f := range []string{filepath.Join(h, ".claude", "settings.json"), filepath.Join(h, ".claude", "skills", "switch"), "every claude session"} {
					if !strings.Contains(out.String(), f) {
						t.Errorf("the question does not name %q: %q", f, out.String())
					}
				}
			}
			if (len(left) == 0) != c.fixed {
				t.Errorf("left %q", left)
			}
			after := findLaunchFix(ctx, adapter.Claude, false, false)
			if (after.hookLine == "" && after.skillLine == "") != c.fixed {
				t.Errorf("after: %+v", after)
			}
			if c.fixed && !strings.Contains(out.String(), "aggy unwire --run") {
				t.Errorf("no undo said: %q", out.String())
			}
			note, _, _ := s.Note(ctx, fixDeclinedNote)
			if note != c.note {
				t.Errorf("note %q, want %q", note, c.note)
			}
		})
	}
}

// TestOwnSkillNotMissing (#142 review M1): a folder of the person's own where
// Claude looks for /switch is theirs, not a missing skill of aggy's; a yes then
// links the other two and fails nothing.
func TestOwnSkillNotMissing(t *testing.T) {
	h, s := fixWorld(t, "y\n", true)
	ctx := context.Background()
	own := filepath.Join(h, ".claude", "skills", "switch")
	if err := os.MkdirAll(own, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "SKILL.md"), []byte("---\nname: switch\n---\nmine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := findLaunchFix(ctx, adapter.Claude, false, false)
	if strings.Contains(f.skillLine, "/switch") || !strings.Contains(f.skillLine, "/borrow") {
		t.Fatalf("skill line %q", f.skillLine)
	}
	var out bytes.Buffer
	if left := offerLaunchFix(ctx, s, adapter.Claude, f, "v1", os.Getenv, offerIn, &out); len(left) != 0 {
		t.Fatalf("left %q: %q", left, out.String())
	}
	if b, err := os.ReadFile(filepath.Join(own, "SKILL.md")); err != nil || !strings.Contains(string(b), "mine") {
		t.Errorf("the person's own skill changed: %q %v", b, err)
	}
}

// TestUnplannedAsksNothing (#142 review M1): when a part that is missing cannot
// be planned, nothing is asked or written, and the next launch of this version
// does not ask either.
func TestUnplannedAsksNothing(t *testing.T) {
	_, s := fixWorld(t, "y\n", true)
	ctx := context.Background()
	f := findLaunchFix(ctx, adapter.Claude, false, false)
	f.planned = false
	var out bytes.Buffer
	if left := offerLaunchFix(ctx, s, adapter.Claude, f, "v1", os.Getenv, offerIn, &out); len(left) == 0 || strings.Contains(out.String(), "[y/N]") {
		t.Fatalf("left %q, out %q", left, out.String())
	}
	if after := findLaunchFix(ctx, adapter.Claude, false, false); after.hookLine == "" {
		t.Error("something was written")
	}
	if !declinedFor(ctx, s, "v1") {
		t.Error("asks again for this version")
	}
}

// TestCodexFixSaysApproval (#142 review M3): Codex runs aggy's hooks only once
// the person approves them, and the question says so.
func TestCodexFixSaysApproval(t *testing.T) {
	_, s := fixWorld(t, "n\n", true)
	ctx := context.Background()
	f := findLaunchFix(ctx, adapter.Codex, false, false)
	var out bytes.Buffer
	offerLaunchFix(ctx, s, adapter.Codex, f, "v1", os.Getenv, offerIn, &out)
	if !strings.Contains(out.String(), codexTrustNote) {
		t.Errorf("no approval said: %q", out.String())
	}
}

// TestSkillUnlinkSilences (#142 review L6): an aggy skill unlink keeps the
// launch from saying the skills are missing, until another version runs.
func TestSkillUnlinkSilences(t *testing.T) {
	_, s := fixWorld(t, "", false)
	ctx := context.Background()
	if code, _, errOut := run("skill", "link", "--vendor", "claude"); code != 0 {
		t.Fatal(errOut)
	}
	if code, _, errOut := run("skill", "unlink", "--vendor", "claude"); code != 0 {
		t.Fatal(errOut)
	}
	if !declinedFor(ctx, s, Version) || declinedFor(ctx, s, Version+"-next") {
		t.Error("the unlink is not kept for this version only")
	}
	if f := findLaunchFix(ctx, adapter.Claude, false, declinedFor(ctx, s, Version)); f.skillLine != "" {
		t.Errorf("skill line %q after an unlink", f.skillLine)
	}
}

// TestAskAtLaunch (#142 review M2): only a new on-screen start asks.
func TestAskAtLaunch(t *testing.T) {
	for _, c := range []struct {
		name string
		t    target
		kind string
		want bool
	}{
		{"a new start", target{}, "", true},
		{"--direct", target{Direct: true}, "", false},
		{"a switch", target{SwitchFrom: "claude"}, "", false},
		{"the same child again (a tool change's resume)", target{SameChild: true}, "", false},
		{"a borrow's child", target{}, "edit", false},
	} {
		if got := askAtLaunch(c.t, c.kind); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// TestLaunchLinesSayWhatIsLost (T6982): each line names what the person loses.
func TestLaunchLinesSayWhatIsLost(t *testing.T) {
	fixWorld(t, "", false)
	for _, c := range []struct {
		v    adapter.Vendor
		want []string
	}{
		{adapter.Claude, []string{"/switch", "not in claude's menu", "aggy skill link"}},
		{adapter.Codex, []string{"$switch", "not in codex's menu"}},
		{adapter.OpenCode, nil},
	} {
		line := findLaunchFix(context.Background(), c.v, true, false).skillLine
		for _, w := range c.want {
			if !strings.Contains(line, w) {
				t.Errorf("%s: %q lacks %q", c.v, line, w)
			}
		}
		if c.want == nil && line != "" {
			t.Errorf("%s: %q", c.v, line)
		}
	}
}
