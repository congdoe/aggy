package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/sessioncmd"
)

// TestSessionToolOnAsks (T6975): aggy tool on run from the in-session page
// asks the first index's y/N there, as at a terminal, instead of indexing with
// no question: the first run answers no and indexes nothing, and its output
// ends the question as the page's y step looks for; the y run indexes. A tool
// with no question turns on at the first run, with nothing for y to answer.
func TestSessionToolOnAsks(t *testing.T) {
	isolate(t)
	fakeCodegraph(t, "0")
	repo := switchRepo(t, "")
	out := sessionAggy([]string{"tool", "on", "codegraph", "--repo", repo}, false)
	if !strings.Contains(out, sessioncmd.Asked) || !strings.Contains(out, "Nothing indexed or turned on.") || exists(t, filepath.Join(repo, ".codegraph")) {
		t.Fatalf("first run: %q", out)
	}
	codegraphOff(t, repo)
	out = sessionAggy([]string{"tool", "on", "codegraph", "--repo", repo}, true)
	if !exists(t, filepath.Join(repo, ".codegraph", indexMarker)) {
		t.Fatalf("y run: %q", out)
	}
	if out := sessionAggy([]string{"tool", "on", "caveman"}, false); strings.Contains(out, sessioncmd.Asked) {
		t.Errorf("a tool with no question asked one: %q", out)
	}
}

// TestOwnStatesCanonical (T6975): a skill aggy wrote under one spelling of
// AGGY_HOME is still aggy's own when AGGY_HOME is spelled another way (here
// through a symlink), not the person's.
func TestOwnStatesCanonical(t *testing.T) {
	skillHome(t)
	mustRun(t, "skill", "link", "--vendor", "claude")
	state := os.Getenv(home.Env)
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{state, link} {
		t.Setenv(home.Env, dir)
		ls, err := ownLayouts()
		if err != nil {
			t.Fatal(err)
		}
		sts, err := ownStates(t.Context(), ls)
		if err != nil {
			t.Fatal(err)
		}
		if st := sts[skillName]; st != ownAggy {
			t.Errorf("AGGY_HOME %s: the aggy skill is %v, want aggy's own", dir, st)
		}
	}
}
