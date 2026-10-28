package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/skills"
)

// skillHome isolates HOME and the state directory, with a Grok home and
// a Grok config the person wrote, and returns the layout.
func skillHome(t *testing.T) (string, skillLayout) {
	t.Helper()
	h := isolate(t)
	writeAt(t, filepath.Join(h, ".grok", "config.toml"), "# mine\r\nmodel = \"grok-x\"\r\n", 0o600)
	writeAt(t, filepath.Join(h, ".claude", "settings.json"), "{}\n", 0o644)
	l, err := layout()
	if err != nil {
		t.Fatal(err)
	}
	return h, l
}

func mustRun(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errOut := run(args...)
	if code != 0 {
		t.Fatalf("aggy %s: exit %d\n%s\n%s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

// TestSkillUnlinkLeavesNothingVisible: with the skill linked for every
// vendor, unlinking one leaves no folder that vendor scans holding a
// skill it would load, keeps it for the others, and aggy unwire --all
// then restores HOME and the state directory exactly.
func TestSkillUnlinkLeavesNothingVisible(t *testing.T) {
	for _, v := range skillVendors {
		t.Run(v, func(t *testing.T) {
			h, l := skillHome(t)
			state := os.Getenv(home.Env)
			before := snapshot(t, h)

			mustRun(t, "skill", "link")
			for _, w := range skillVendors {
				vis, err := l.visible(w)
				if err != nil || len(vis) == 0 {
					t.Fatalf("after link, %s sees %v, %v", w, vis, err)
				}
			}
			if b, _ := os.ReadFile(filepath.Join(l.store, "SKILL.md")); string(b) != string(skills.Aggy) {
				t.Error("the installed skill is not the embedded one")
			}

			mustRun(t, "skill", "unlink", "--vendor", v)
			for _, w := range skillVendors {
				vis, err := l.visible(w)
				if err != nil {
					t.Fatal(err)
				}
				if w == v && len(vis) > 0 {
					t.Errorf("after unlink --vendor %s, it still loads the skill through %v", v, vis)
				}
				if w != v && len(vis) == 0 {
					t.Errorf("unlink --vendor %s took the skill from %s too", v, w)
				}
			}
			b, _ := os.ReadFile(l.grokConfig)
			if got := strings.Contains(string(b), l.store); got != (v == "grok") {
				t.Errorf("grok ignore entry present = %v after unlinking %s:\n%s", got, v, b)
			}

			code, out, errOut := run("unwire", "--all")
			if code != 0 {
				t.Fatalf("unwire: %d\n%s\n%s", code, out, errOut)
			}
			if d := diffSnapshots(before, snapshot(t, h)); len(d) > 0 {
				t.Errorf("unwire did not restore HOME:\n%s", strings.Join(d, "\n"))
			}
			after := snapshot(t, state)
			for p := range after {
				if strings.HasPrefix(p, "skills") {
					t.Errorf("unwire left %s in the state directory", p)
				}
			}
		})
	}
}

// TestSkillGrokRelinkDropsIgnore: Grok off and on again leaves its
// config byte for byte as the person wrote it; with no other link, Grok
// off needs no ignore entry at all.
func TestSkillGrokRelinkDropsIgnore(t *testing.T) {
	_, l := skillHome(t)
	orig, _ := os.ReadFile(l.grokConfig)
	mustRun(t, "skill", "link")
	mustRun(t, "skill", "unlink", "--vendor", "grok")
	mustRun(t, "skill", "link", "--vendor", "grok")
	if b, _ := os.ReadFile(l.grokConfig); string(b) != string(orig) {
		t.Errorf("grok config = %q, want %q", b, orig)
	}
	mustRun(t, "skill", "unlink")
	if b, _ := os.ReadFile(l.grokConfig); string(b) != string(orig) {
		t.Errorf("with nothing linked, grok config = %q, want %q", b, orig)
	}
	for _, v := range skillVendors {
		if vis, _ := l.visible(v); len(vis) > 0 {
			t.Errorf("%s still sees %v", v, vis)
		}
	}
}

// TestSkillDryRun: --dry-run shows the changes and writes nothing, not
// even the index.
func TestSkillDryRun(t *testing.T) {
	h, l := skillHome(t)
	before := snapshot(t, h)
	out := mustRun(t, "skill", "link", "--dry-run")
	for _, want := range []string{l.links["claude"] + ": nothing -> symlink to " + l.store, "+---"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run lacks %q:\n%s", want, out)
		}
	}
	if d := diffSnapshots(before, snapshot(t, h)); len(d) > 0 {
		t.Errorf("dry run changed HOME: %v", d)
	}
	if p, _ := store.DefaultPath(); p != "" {
		if _, err := os.Stat(p); err == nil {
			t.Error("dry run created the index")
		}
	}
	if _, err := os.Stat(l.store); err == nil {
		t.Error("dry run installed the skill")
	}
}

// TestSkillLinkRefusesForeignEntry: an aggy entry aggy did not make is
// not replaced.
func TestSkillLinkRefusesForeignEntry(t *testing.T) {
	h, _ := skillHome(t)
	writeAt(t, filepath.Join(h, ".agents", "skills", "aggy", "SKILL.md"), "mine\n", 0o644)
	before := snapshot(t, h)
	if code, _, errOut := run("skill", "link"); code == 0 || !strings.Contains(errOut, "not aggy's link") {
		t.Errorf("link over a foreign entry: exit %d, %q", code, errOut)
	}
	if d := diffSnapshots(before, snapshot(t, h)); len(d) > 0 {
		t.Errorf("a refused link changed HOME: %v", d)
	}
}

// TestGrokIgnoreTransforms: adding and removing the ignore entry as text
// edits, refusing what aggy cannot change safely.
func TestGrokIgnoreTransforms(t *testing.T) {
	const entry = "/s/aggy"
	tests := []struct {
		name, src, added string
		addErr           bool
	}{
		{"no file", "", "[skills]\nignore = ['/s/aggy']\n", false},
		{"no skills table", "a = 1\n", "a = 1\n\n[skills]\nignore = ['/s/aggy']\n", false},
		{"no trailing newline, CRLF", "a = 1\r\n[x]\r\nb = 2", "a = 1\r\n[x]\r\nb = 2\r\n[skills]\r\nignore = ['/s/aggy']\r\n", false},
		{"no trailing newline", "x = 1", "x = 1\n[skills]\nignore = ['/s/aggy']\n", false},
		{"skills table without ignore", "[skills]\npaths = [\"~/t\"]\n[y]\n", "[skills]\nignore = ['/s/aggy']\npaths = [\"~/t\"]\n[y]\n", false},
		{"ignore list already", "[skills]\nignore = [\"~/wip\"]\n", "", true},
		{"dotted keys", "skills.paths = [\"~/t\"]\n", "", true},
		{"not TOML", "a = \n", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var src []byte
			if tt.src != "" {
				src = []byte(tt.src)
			}
			got, err := addGrokIgnore(entry)(src)
			if tt.addErr {
				if err == nil {
					t.Errorf("add = %q, want an error", got)
				}
				return
			}
			if err != nil || string(got) != tt.added {
				t.Fatalf("add = %q, %v, want %q", got, err, tt.added)
			}
			if again, _ := addGrokIgnore(entry)(got); string(again) != string(got) {
				t.Error("add is not idempotent")
			}
			back, err := removeGrokIgnore(entry)(got)
			if err != nil {
				t.Fatal(err)
			}
			want := tt.src
			if tt.src == "" {
				want = "[skills]\nignore = ['/s/aggy']\n" // a whole-file block goes by a removal patch instead
				back = got
			}
			if string(back) != want {
				t.Errorf("remove = %q, want %q", back, want)
			}
		})
	}
	if _, err := removeGrokIgnore(entry)([]byte("[skills]\nignore = ['/s/aggy', 'x']\n")); err == nil {
		t.Error("removing from an edited list succeeded")
	}
}

// TestSkillStatusShowsCost: status reports the token estimate.
func TestSkillStatusShowsCost(t *testing.T) {
	skillHome(t)
	always, body := skills.Estimate(skills.Aggy)
	out := mustRun(t, "skill", "status")
	want := fmt.Sprintf("about %d tokens every turn in a CLI that loads it, %d more when invoked", always, body)
	if !strings.Contains(out, want) || !strings.Contains(out, "claude: not linked; loads nothing") {
		t.Errorf("status = %q", out)
	}
}

// TestSkillGrokIgnoreNoTrailingNewline (B14-3): a Grok config without a
// final line ending comes back byte for byte after Grok is off (only
// Claude linked) and then linked again.
func TestSkillGrokIgnoreNoTrailingNewline(t *testing.T) {
	h, l := skillHome(t)
	writeAt(t, filepath.Join(h, ".grok", "config.toml"), "x = 1", 0o600)
	mustRun(t, "skill", "link", "--vendor", "claude")
	if b, _ := os.ReadFile(l.grokConfig); !strings.Contains(string(b), l.store) {
		t.Fatalf("no ignore entry while only Claude is linked: %q", b)
	}
	mustRun(t, "skill", "link", "--vendor", "grok")
	if b, _ := os.ReadFile(l.grokConfig); string(b) != "x = 1" {
		t.Errorf("grok config = %q, want %q", b, "x = 1")
	}
}
