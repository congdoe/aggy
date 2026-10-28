package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// switchRepo makes a git work tree (a .git folder is enough for
// repoRoot) with the given .aggy/config.toml, or none.
func switchRepo(t *testing.T, cfg string) string {
	t.Helper()
	r := t.TempDir()
	if err := os.Mkdir(filepath.Join(r, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if cfg != "" {
		writeAt(t, filepath.Join(r, ".aggy", "config.toml"), cfg, 0o600)
	}
	return r
}

func statusLine(t *testing.T, out, tool string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 0 && f[0] == tool {
			return strings.Join(f, " ")
		}
	}
	t.Fatalf("status has no %s line:\n%s", tool, out)
	return ""
}

// TestToolOnOff: settings per vendor and repo; the newest, broadest
// setting holds wherever it applies; status says why.
func TestToolOnOff(t *testing.T) {
	_, dest := toolHome(t, demoTool)
	writeAt(t, dest, demoTool, 0o600)
	repo := switchRepo(t, "")
	t.Chdir(repo)

	steps := []struct {
		args []string
		want string // the status line for demo: claude codex grok
	}{
		{nil, "demo off off off"},
		{[]string{"on", "demo", "--vendor", "claude"}, "demo on off off"},
		{[]string{"on", "demo", "--repo", "."}, "demo on on on"},
		{[]string{"off", "demo", "--vendor=codex", "--repo", repo}, "demo on off on"},
		{[]string{"off", "demo"}, "demo off off off"},
	}
	for _, s := range steps {
		if s.args != nil {
			if code, _, errOut := toolRun("", false, s.args...); code != 0 {
				t.Fatalf("%v: exit %d %q", s.args, code, errOut)
			}
		}
		code, out, errOut := toolRun("", false, "status")
		if code != 0 {
			t.Fatalf("status: exit %d %q", code, errOut)
		}
		if got := statusLine(t, out, "demo"); !strings.HasPrefix(got, s.want) {
			t.Errorf("after %v: %q, want %q", s.args, got, s.want)
		}
	}
	// Elsewhere, only settings for every repo apply.
	t.Chdir(t.TempDir())
	if code, _, _ := toolRun("", false, "on", "demo", "--repo", repo); code != 0 {
		t.Fatal("on --repo")
	}
	_, out, _ := toolRun("", false, "status")
	if got := statusLine(t, out, "demo"); !strings.HasPrefix(got, "demo off off off") {
		t.Errorf("outside the repo: %q", got)
	}
}

// TestToolRepoCannotTurnOn is the security test: a cloned repo's config
// can only turn tools off. tools_off beats the person's own setting, a
// key that would turn one on is an error, and a repo config that cannot
// be read refuses rather than allowing.
func TestToolRepoCannotTurnOn(t *testing.T) {
	_, dest := toolHome(t, demoTool)
	writeAt(t, dest, demoTool, 0o600)

	off := switchRepo(t, `tools_off = ["demo", "not-a-known-tool"]`)
	t.Chdir(off)
	if code, _, errOut := toolRun("", false, "on", "demo"); code != 0 {
		t.Fatalf("on: exit %d %q", code, errOut)
	}
	_, out, _ := toolRun("", false, "status")
	if got := statusLine(t, out, "demo"); !strings.HasPrefix(got, "demo off off off") || !strings.Contains(got, "tools_off") {
		t.Errorf("tools_off: %q", got)
	}
	code, _, errOut := toolRun("", false, "on", "demo", "--repo", ".")
	if code != 1 || !strings.Contains(errOut, "tool.repo_off") {
		t.Errorf("on in a tools_off repo: exit %d %q", code, errOut)
	}

	for _, cfg := range []string{
		`tools_on = ["demo"]`,
		"[tools]\ndemo = true\n",
		"[tools.demo]\non = true\n",
		`tools_off = "demo"`,
		`tools_off = ["../x"]`,
	} {
		r := switchRepo(t, cfg)
		t.Chdir(r)
		code, out, errOut := toolRun("", false, "status")
		if code != 1 || !strings.Contains(errOut, "cli.config_invalid") || strings.Contains(out, " on ") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", cfg, code, out, errOut)
		}
	}
}

// TestToolOnRefuses: unknown tools, vendors a tool has nothing for, a
// per-repo tool without --repo, and --repo outside a work tree.
func TestToolOnRefuses(t *testing.T) {
	_, dest := toolHome(t, demoTool)
	writeAt(t, dest, demoTool, 0o600)
	solo := strings.NewReplacer(`name = "demo"`, `name = "solo"`, `"DEMO_HOME"`, `"SOLO_HOME"`).Replace(demoTool[:strings.Index(demoTool, "[[hook]]")]) + "[cost]\nmethod = \"none\"\n"
	writeAt(t, filepath.Join(filepath.Dir(dest), "solo.toml"), solo, 0o600)
	t.Chdir(switchRepo(t, ""))
	for _, c := range []struct {
		args []string
		want string
		exit int
	}{
		{[]string{"on", "nope"}, "tool.unknown", 1},
		{[]string{"on", "solo", "--vendor", "grok"}, "tool.not_applicable", 1},
		{[]string{"on", "codegraph"}, "tool.not_applicable", 1},
		{[]string{"on", "demo", "--repo", t.TempDir()}, "tool.not_applicable", 1},
		{[]string{"on", "demo", "--vendor", "gemini"}, "cli.usage", 1},
		{[]string{"on", "demo", "--vendor"}, "cli.usage", 2},
		{[]string{"on"}, "cli.usage", 2},
		{[]string{"on", "demo", "--yes"}, "cli.usage", 2},
		{[]string{"list", "--vendor", "claude"}, "cli.usage", 2},
	} {
		code, _, errOut := toolRun("", false, c.args...)
		if code != c.exit || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: exit %d %q, want %d %s", c.args, code, errOut, c.exit, c.want)
		}
	}
	// A per-repo tool turns on with --repo.
	if code, out, errOut := toolRun("", false, "on", "codegraph", "--repo", "."); code != 0 || !strings.Contains(out, "codegraph for claude") {
		t.Errorf("codegraph --repo: exit %d %q %q", code, out, errOut)
	}
}
