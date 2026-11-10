package cli

import (
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/skills"
)

// TestRefreshLeavesOthersSkills (R121): a launch never changes a SKILL.md
// aggy did not write. A skill the person stored under a name that is now
// aggy's (as aggy skill add allowed before T6910) is left as it is, said once,
// kept out of aggy skill link and unlink, and stays removable; a copy aggy
// wrote and the person then edited is not overwritten either.
func TestRefreshLeavesOthersSkills(t *testing.T) {
	const mine = "---\nname: inbox\ndescription: my mail triage skill\n---\nmy own body\n"
	tests := []struct {
		name  string
		setup func(t *testing.T, h, state string) (file, want string)
		note  string
		after func(t *testing.T, h, state string)
	}{
		{
			name: "a skill of the person's named inbox, with its tool",
			setup: func(t *testing.T, h, state string) (string, string) {
				dir := filepath.Join(state, "skills", "inbox")
				writeAt(t, filepath.Join(dir, "SKILL.md"), mine, 0o644)
				writeAt(t, filepath.Join(state, "tools", "inbox.toml"), string(skillDefFor("inbox", "my mail triage skill", strings.Repeat("a", 64))), 0o600)
				symlinkAt(t, dir, filepath.Join(h, ".claude", "skills", "inbox"))
				return filepath.Join(dir, "SKILL.md"), mine
			},
			note: "holds a skill of yours",
			after: func(t *testing.T, h, state string) {
				dir := filepath.Join(state, "skills", "inbox")
				link := filepath.Join(h, ".claude", "skills", "inbox")
				lout := mustRun(t, "skill", "link")
				if b, _ := os.ReadFile(filepath.Join(dir, "SKILL.md")); string(b) != mine || !strings.Contains(lout, "holds a skill of yours") {
					t.Errorf("link: %q\n%q", b, lout)
				}
				mustRun(t, "skill", "unlink")
				if dst, err := os.Readlink(link); err != nil || dst != dir {
					t.Errorf("the person's link = %q %v", dst, err)
				}
				// Still the person's to remove, once their link is gone.
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if code, out, errOut := skillRun("", false, "remove", "inbox"); code != 0 {
					t.Errorf("remove: %d %q %q", code, out, errOut)
				}
				// A new one may not take the name.
				src := filepath.Join(t.TempDir(), "inbox")
				writeAt(t, filepath.Join(src, "SKILL.md"), mine, 0o644)
				if code, _, errOut := skillRun("", true, "add", src, "--yes"); code == 0 || !strings.Contains(errOut, "one of aggy's own") {
					t.Errorf("add inbox: %d %q", code, errOut)
				}
			},
		},
		{
			name: "a folder named inbox aggy never wrote, no tool",
			setup: func(t *testing.T, h, state string) (string, string) {
				dir := filepath.Join(state, "skills", "inbox")
				writeAt(t, filepath.Join(dir, "SKILL.md"), mine, 0o644)
				symlinkAt(t, dir, filepath.Join(h, ".claude", "skills", "inbox"))
				return filepath.Join(dir, "SKILL.md"), mine
			},
			note: "holds a skill of yours",
		},
		{
			name: "aggy's copy edited by hand",
			setup: func(t *testing.T, h, state string) (string, string) {
				mustRun(t, "skill", "link", "--vendor", "claude")
				file := filepath.Join(state, "skills", "aggy", "SKILL.md")
				edited := string(skills.Aggy) + "\nmy own line\n"
				if err := os.WriteFile(file, []byte(edited), 0o644); err != nil {
					t.Fatal(err)
				}
				return file, edited
			},
			note: "changed by hand",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, _ := skillHome(t)
			state := os.Getenv(home.Env)
			file, want := tt.setup(t, h, state)
			var out strings.Builder
			refreshSkills(t.Context(), &out)
			if b, _ := os.ReadFile(file); string(b) != want {
				t.Fatalf("a launch replaced %s (%q): %q", file, out.String(), b)
			}
			if !strings.Contains(out.String(), tt.note) {
				t.Errorf("note = %q, want %q", out.String(), tt.note)
			}
			var again strings.Builder
			refreshSkills(t.Context(), &again)
			if again.Len() > 0 {
				t.Errorf("said twice: %q", again.String())
			}
			if tt.after != nil {
				tt.after(t, h, state)
			}
		})
	}
}

// writeAsAggy writes content to file through the manifest, as aggy itself
// does.
func writeAsAggy(t *testing.T, file, content string) {
	t.Helper()
	h, err := patch.HashFile(file)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.OpenDefault(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		t.Fatal(err)
	}
	p := patch.Patch{File: file, ExpectedHash: h, Mode: 0o644, Transform: func([]byte) ([]byte, error) { return []byte(content), nil }}
	if _, err := m.Run(t.Context(), manifest.Write, manifest.NewRunID(), []patch.Patch{p}, io.Discard); err != nil {
		t.Fatal(err)
	}
}

// symlinkAt makes link point at target, with link's folder.
func symlinkAt(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// TestOwnSkillCost (R121): setup's cost line counts every linked skill's
// frontmatter per CLI: on Codex the command skills' too, which no recorded
// key keeps out of its context.
func TestOwnSkillCost(t *testing.T) {
	skillHome(t)
	ls, err := ownLayouts()
	if err != nil {
		t.Fatal(err)
	}
	aggy, _ := skills.Estimate(skills.Aggy)
	all := 0
	for _, sk := range skills.Own() {
		a, _ := skills.Estimate(sk.Content)
		all += a
	}
	tests := []struct {
		vendors []string
		want    string
	}{
		{[]string{"claude"}, "about " + strconv.Itoa(aggy) + " tokens every turn in claude"},
		{[]string{"codex"}, "about " + strconv.Itoa(all) + " tokens every turn in codex"},
		{[]string{"codex", "grok"}, "about " + strconv.Itoa(all) + " tokens every turn in codex, about " + strconv.Itoa(aggy) + " tokens every turn in grok"},
	}
	for _, tt := range tests {
		if got := ownSkillCost(ls, tt.vendors); got != tt.want {
			t.Errorf("ownSkillCost(%v) = %q, want %q", tt.vendors, got, tt.want)
		}
	}
	if all <= aggy {
		t.Fatalf("the command skills add nothing: %d <= %d", all, aggy)
	}
}
