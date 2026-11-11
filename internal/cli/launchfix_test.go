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

// TestOfferLaunchFix (T6982): a launch that finds aggy's hooks or command
// skills missing for the CLI asks once, with one key, to add them: a yes adds
// both through the manifest, so the launch has nothing left to say and aggy
// unwire takes them out; a no is kept for this aggy version and asked again
// only by another; with nobody to ask, it only says what is missing.
func TestOfferLaunchFix(t *testing.T) {
	const bin = "/opt/homebrew/bin/aggy"
	for _, c := range []struct {
		name     string
		typed    string
		term     bool
		declined string // the note before
		asked    bool
		fixed    bool
		note     string // the note after
	}{
		{"yes", "y\n", true, "", true, true, ""},
		{"no", "n\n", true, "", true, false, "v1"},
		{"no terminal", "y\n", false, "", false, false, ""},
		{"no given for this version", "y\n", true, "v1", false, false, "v1"},
		{"no given for another version", "y\n", true, "v0", true, true, "v0"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := isolate(t)
			oldBin, oldPath, oldCheck := hookBin, pathAggy, checkTrusted
			hookBin = func() (string, error) { return bin, nil }
			pathAggy = func() (string, error) { return bin, nil }
			checkTrusted = func(string) error { return nil }
			t.Cleanup(func() { hookBin, pathAggy, checkTrusted = oldBin, oldPath, oldCheck })
			if err := os.MkdirAll(filepath.Join(h, ".claude"), 0o700); err != nil {
				t.Fatal(err)
			}
			inPlaceWorld(t, c.typed, c.term, false)
			ctx := context.Background()
			s, err := store.OpenDefault(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			if c.declined != "" {
				if err := s.SetNote(ctx, fixDeclinedNote, c.declined, time.Now()); err != nil {
					t.Fatal(err)
				}
			}
			f := findLaunchFix(ctx, adapter.Claude, false)
			if f.hookLine == "" || f.skillLine == "" || len(f.hooks) == 0 || len(f.skills) != 3 {
				t.Fatalf("found %+v", f)
			}
			var out bytes.Buffer
			left := offerLaunchFix(ctx, s, adapter.Claude, f, "v1", os.Getenv, offerIn, &out)
			if got := strings.Contains(out.String(), "[y/N]"); got != c.asked {
				t.Errorf("asked %v, want %v: %q", got, c.asked, out.String())
			}
			if (len(left) == 0) != c.fixed {
				t.Errorf("left %q", left)
			}
			after := findLaunchFix(ctx, adapter.Claude, false)
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

// TestLaunchLinesSayWhatIsLost (T6982): each line names what the person loses.
func TestLaunchLinesSayWhatIsLost(t *testing.T) {
	isolate(t)
	oldBin, oldPath, oldCheck := hookBin, pathAggy, checkTrusted
	hookBin = func() (string, error) { return "/opt/homebrew/bin/aggy", nil }
	pathAggy = hookBin
	checkTrusted = func(string) error { return nil }
	t.Cleanup(func() { hookBin, pathAggy, checkTrusted = oldBin, oldPath, oldCheck })
	for _, c := range []struct {
		v    adapter.Vendor
		want []string
	}{
		{adapter.Claude, []string{"/switch", "not in claude's menu", "aggy skill link"}},
		{adapter.Codex, []string{"$switch", "not in codex's menu"}},
		{adapter.OpenCode, nil},
	} {
		_, line := missingSkills(context.Background(), c.v)
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
