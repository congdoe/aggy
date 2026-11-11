package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/config"
)

// TestCLIMissing (T6979): a vendor whose CLI is not on PATH is said to be not
// installed, with where to get it and another vendor to start; at a terminal
// the installed ones are offered, none by default.
func TestCLIMissing(t *testing.T) {
	err := cliMissing(adapter.Grok, []adapter.Vendor{adapter.Claude})
	for _, w := range []string{"grok is not installed", "finds no grok on PATH", "github.com/xai-org/grok-build", "aggy claude"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("%q lacks %q", err, w)
		}
	}
	for _, v := range adapter.Known() {
		if installPage(v) == "" && v != adapter.Vendor("antigravity") {
			t.Errorf("%s has no install page", v)
		}
	}
	isolate(t)
	old := lookPathFor
	lookPathFor = func(v adapter.Vendor) bool { return v == adapter.Claude || v == adapter.Codex }
	t.Cleanup(func() { lookPathFor = old })
	others := installedElse(config.Default(), t.TempDir(), adapter.Grok)
	if len(others) != 2 || others[0] != adapter.Claude && others[1] != adapter.Claude {
		t.Fatalf("installed: %q", others)
	}
	// #133 review: the picked vendor starts with the policy on, whatever
	// flags the missing one was given, and the pick says so.
	if nt := pickedTarget(adapter.Claude, target{Vendor: adapter.Grok, NoPolicy: true, Direct: true}); nt.NoPolicy || !nt.New || nt.Vendor != adapter.Claude || !nt.Direct {
		t.Errorf("picked target %+v", nt)
	}
	for _, c := range []struct {
		name, typed string
		term, shell bool
		want        adapter.Vendor
	}{
		{"a pick", "2\n", true, false, others[1]},
		{"Enter picks none", "\n", true, false, ""},
		{"a word picks none", "codex\n", true, false, ""},
		{"out of range", "9\n", true, false, ""},
		{"a paste", "\x1b[200~1\x1b[201~\n", true, false, ""},
		{"no terminal", "1\n", false, false, ""},
		{"a session's tool shell", "1\n", true, true, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			inPlaceWorld(t, c.typed, c.term, c.shell)
			var out bytes.Buffer
			if got := pickInstead(context.Background(), adapter.Grok, others, offerIn, &out); got != c.want {
				t.Errorf("picked %q, want %q: %q", got, c.want, out.String())
			}
			if c.term && !c.shell && !strings.Contains(out.String(), "with aggy's safety policy on") {
				t.Errorf("the pick does not say the policy is on: %q", out.String())
			}
		})
	}
}

// TestRefusalWords (T6979): the refusals that send the person elsewhere name
// where: a subcommand word, the repo's allowed_vendors file and key, and the
// trust that untrusted rules wait for.
func TestRefusalWords(t *testing.T) {
	_, err := claude.Adapter{}.PassFlags().Parse([]string{"agents"}, false)
	if !errors.Is(err, adapter.ErrNotFlag) || !strings.Contains(err.Error(), "run the CLI itself for that subcommand") || !strings.Contains(err.Error(), "put the prompt after --") {
		t.Errorf("subcommand word: %v", err)
	}
	isolate(t)
	repo := tempRepo(t)
	writeAt(t, filepath.Join(repo, ".aggy", "config.toml"), "allowed_vendors = [\"claude\"]\n", 0o600)
	_, err = allowedIn(config.Default(), repo, adapter.Grok)
	if !errors.Is(err, errNotAllowed) || !strings.Contains(err.Error(), "allowed_vendors key in "+filepath.Join(repo, ".aggy", "config.toml")) || !strings.Contains(err.Error(), "add grok there") {
		t.Errorf("allowed_vendors: %v", err)
	}
	if n := skippedNote(2, "Claude Code", true); !strings.Contains(n, "aggy trust in a terminal, or /aggy trust in the session") {
		t.Errorf("untrusted: %q", n)
	}
	if n := skippedNote(1, "Claude Code", false); strings.Contains(n, "aggy trust") || !strings.Contains(n, "1 of your permission rules does not apply") {
		t.Errorf("trusted: %q", n)
	}
}

// TestAcceptProtectedAsks (T6979): a diff that touches a file that may hold
// hooks, CI, config, or instructions is shown, with its hunks, and asked
// about at a terminal; yes applies it through every other check, no applies
// nothing; with no terminal it is refused, with the commands to do it by
// hand; a .git path is refused whatever the answer.
func TestAcceptProtectedAsks(t *testing.T) {
	f, markers := editFixture(t)
	gitIn(t, f.repo, "checkout", "--", "a.go")
	files := map[string]string{"a.go": "package a\n", ".github/workflows/ci.yml": "on: push # SEEN\n"}
	t.Run("no terminal", func(t *testing.T) {
		inPlaceWorld(t, "", false, false)
		id, wt := editBorrow(t, f, files)
		code, out, errOut := accept(f, "", false, id, "--yes")
		if code != 1 || !strings.Contains(errOut, ".github/workflows/ci.yml") || !strings.Contains(errOut, "by hand: git -C "+wt) || !strings.Contains(errOut, "aggy reject "+id) {
			t.Errorf("code %d: %s %s", code, out, errOut)
		}
	})
	t.Run("no", func(t *testing.T) {
		inPlaceWorld(t, "", true, false)
		id, _ := editBorrow(t, f, files)
		code, out, _ := accept(f, "n\n", true, id)
		if code != 1 || !strings.Contains(out, "on: push # SEEN") || !strings.Contains(out, "Accept anyway, these files included? [y/N]") || !strings.Contains(out, "Not applied.") {
			t.Errorf("code %d: %s", code, out)
		}
		if _, err := os.Stat(filepath.Join(f.repo, ".github", "workflows", "ci.yml")); err == nil {
			t.Error("applied after a no")
		}
	})
	t.Run("a session's tool shell", func(t *testing.T) {
		inPlaceWorld(t, "", true, true)
		id, _ := editBorrow(t, f, files)
		if code, out, errOut := accept(f, "y\ny\n", true, id); code != 1 || !strings.Contains(errOut, "by hand") {
			t.Errorf("code %d: %s %s", code, out, errOut)
		}
	})
	t.Run("yes", func(t *testing.T) {
		inPlaceWorld(t, "", true, false)
		id, _ := editBorrow(t, f, files)
		code, out, errOut := accept(f, "y\ny\n", true, id)
		if code != 0 {
			t.Fatalf("code %d: %s %s", code, out, errOut)
		}
		if b, err := os.ReadFile(filepath.Join(f.repo, ".github", "workflows", "ci.yml")); err != nil || !strings.Contains(string(b), "SEEN") {
			t.Errorf("not applied: %v", err)
		}
	})
	// #133 review: a borrow child picks the names, so a control character in
	// one is escaped in the summary above the question and in the refusal.
	t.Run("a name with control characters", func(t *testing.T) {
		evil := ".github/workflows/x\x1b[2K\rAll clear.yml"
		for _, term := range []bool{true, false} {
			inPlaceWorld(t, "", term, false)
			id, _ := editBorrow(t, f, map[string]string{"a.go": "package a\n", evil: "on: push\n"})
			code, out, errOut := accept(f, "n\n", term, id)
			if code != 1 || strings.ContainsAny(out+errOut, "\x1b\r") || !strings.Contains(out+errOut, "All clear.yml") {
				t.Errorf("terminal %v: code %d: %q %q", term, code, out, errOut)
			}
		}
	})
	// #133 re-review: a name that reads as pathspec magic must not hide the
	// other files' hunks: the question shows ci.yml's, or is not asked.
	t.Run("a name that reads as pathspec magic", func(t *testing.T) {
		inPlaceWorld(t, "", true, false)
		magic := map[string]string{"a.go": "package a\n", ".github/workflows/ci.yml": "on: push # SEEN\n", ":(exclude,glob)**/.github/**": "x\n"}
		id, _ := editBorrow(t, f, magic)
		code, out, errOut := accept(f, "n\n", true, id)
		if code != 1 || !strings.Contains(out, "on: push # SEEN") || !strings.Contains(out, "Accept anyway") {
			t.Errorf("code %d: %s %s", code, out, errOut)
		}
	})
	t.Run("a .git path is refused", func(t *testing.T) {
		inPlaceWorld(t, "", true, false)
		if _, err := checkTouched(":100644 100644 0 0 M\x00sub/.git/config\x00", ""); err == nil || protectedOf(err) != nil || !strings.Contains(err.Error(), "inside a .git folder") {
			t.Errorf("err %v", err)
		}
	})
	noMarkers(t, markers)
}
