package cli

import (
	"context"
	"time"

	"github.com/congdoe/aggy/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/home"
)

// runGit runs git in dir, with no system or user config.
func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// switchRepo makes a git repo with one commit and the given
// .aggy/config.toml (untracked), or none.
func switchRepo(t *testing.T, cfg string) string {
	t.Helper()
	r := t.TempDir()
	runGit(t, r, "init", "-q")
	runGit(t, r, "commit", "-q", "--allow-empty", "-m", "one")
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

	// A config that cannot be read: on refuses, status warns and shows
	// every tool off there (S63-5), and off still works.
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
		if code != 0 || !strings.Contains(errOut, "warning") || !strings.HasPrefix(statusLine(t, out, "demo"), "demo off off off") {
			t.Errorf("status %q: exit %d, stdout %q, stderr %q", cfg, code, out, errOut)
		}
		if code, _, errOut := toolRun("", false, "on", "demo", "--repo", "."); code != 1 || !strings.Contains(errOut, "cli.config_invalid") {
			t.Errorf("on %q: exit %d %q", cfg, code, errOut)
		}
		if code, _, errOut := toolRun("", false, "off", "demo", "--repo", "."); code != 0 {
			t.Errorf("off %q: exit %d %q", cfg, code, errOut)
		}
	}
}

// TestToolSwitchWorktree (S63-2): a repo setting, and the main
// checkout's tools_off, follow the repo into a linked worktree, and the
// worktree's own tools_off adds to them.
func TestToolSwitchWorktree(t *testing.T) {
	_, dest := toolHome(t, demoTool)
	writeAt(t, dest, demoTool, 0o600)
	main := switchRepo(t, "")
	wt := filepath.Join(t.TempDir(), "wt")
	runGit(t, main, "worktree", "add", "-q", wt)

	steps := []struct {
		args []string
		dir  string
		want string
	}{
		{[]string{"on", "demo"}, wt, "demo on on on"},
		{[]string{"off", "demo", "--repo", main}, wt, "demo off off off"},
		{[]string{"on", "demo", "--repo", wt, "--vendor", "codex"}, main, "demo off on off"},
		{[]string{"on", "codegraph", "--repo", main}, wt, "codegraph on on on"},
	}
	for _, s := range steps {
		if code, _, errOut := toolRun("", false, s.args...); code != 0 {
			t.Fatalf("%v: exit %d %q", s.args, code, errOut)
		}
		_, out, _ := toolRun("", false, "status", "--repo", s.dir)
		f := strings.Fields(s.want)
		if got := statusLine(t, out, f[0]); !strings.HasPrefix(got, s.want) {
			t.Errorf("after %v, in %s: %q, want %q", s.args, s.dir, got, s.want)
		}
	}
	// tools_off: the main checkout's applies in the worktree, and the
	// worktree's own adds to it.
	writeAt(t, filepath.Join(main, ".aggy", "config.toml"), `tools_off = ["codegraph"]`, 0o600)
	writeAt(t, filepath.Join(wt, ".aggy", "config.toml"), `tools_off = ["demo"]`, 0o600)
	_, out, _ := toolRun("", false, "status", "--repo", wt)
	for _, tool := range []string{"codegraph", "demo"} {
		if got := statusLine(t, out, tool); !strings.Contains(got, "off off off") {
			t.Errorf("worktree %s: %q", tool, got)
		}
	}
	if _, out, _ := toolRun("", false, "status", "--repo", main); !strings.HasPrefix(statusLine(t, out, "demo"), "demo off on off") {
		t.Errorf("the worktree's tools_off reached the main checkout:\n%s", out)
	}
}

// TestToolSwitchStale (S63-1): an "on" never carries to another or an
// edited definition, and a refused definition can be turned off and
// removed.
func TestToolSwitchStale(t *testing.T) {
	src, dest := toolHome(t, demoTool)
	t.Chdir(t.TempDir())
	if code, _, errOut := toolRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatalf("add: %d %q", code, errOut)
	}
	if code, _, errOut := toolRun("", false, "on", "demo"); code != 0 {
		t.Fatalf("on: %d %q", code, errOut)
	}
	// A: the file removed by hand, and another demo added.
	if err := os.Remove(dest); err != nil {
		t.Fatal(err)
	}
	writeAt(t, src, strings.Replace(demoTool, `"--stdio"`, `"--other"`, 1), 0o644)
	if code, _, errOut := toolRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatalf("add again: %d %q", code, errOut)
	}
	if _, out, _ := toolRun("", false, "status"); !strings.HasPrefix(statusLine(t, out, "demo"), "demo off off off") {
		t.Errorf("a new demo took the old one's on:\n%s", out)
	}
	// An edit after on turns it off, and says why.
	if code, _, _ := toolRun("", false, "on", "demo"); code != 0 {
		t.Fatal("on")
	}
	writeAt(t, dest, demoTool+"\n# edited\n", 0o600)
	if _, out, _ := toolRun("", false, "status"); !strings.HasPrefix(statusLine(t, out, "demo"), "demo off off off") || !strings.Contains(out, "changed since") {
		t.Errorf("an edited definition stayed on:\n%s", out)
	}
	// S63-16: on turns on only the file aggy tool add showed.
	if code, _, errOut := toolRun("", false, "on", "demo"); code != 1 || !strings.Contains(errOut, "tool.changed_since_add") {
		t.Errorf("on an edited file: %d %q", code, errOut)
	}
	// B: an invalid file can be turned off and removed.
	writeAt(t, dest, "name = 1\n", 0o600)
	if code, out, errOut := toolRun("", false, "off", "demo"); code != 0 || !strings.Contains(out, "not loaded") {
		t.Errorf("off a refused tool: %d %q %q", code, out, errOut)
	}
	if code, _, _ := toolRun("", false, "on", "demo"); code != 1 {
		t.Error("on a refused tool")
	}
	if code, _, errOut := toolRun("", false, "remove", "demo"); code != 0 || exists(t, dest) {
		t.Errorf("remove a refused tool: %d %q", code, errOut)
	}
}

// TestToolRepoLinks: a crafted .git that names another repo keys the
// work tree by its own root, so that repo's settings and tools_off never
// apply (S63-11); and off and status take a broken .git, which only on
// refuses (S63-17).
func TestToolRepoLinks(t *testing.T) {
	_, dest := toolHome(t, demoTool)
	writeAt(t, dest, demoTool, 0o600)
	other := switchRepo(t, "")
	if code, _, errOut := toolRun("", false, "on", "demo", "--repo", other); code != 0 {
		t.Fatalf("on in other: %d %q", code, errOut)
	}
	crafted := t.TempDir()
	writeAt(t, filepath.Join(crafted, ".git"), "gitdir: "+filepath.Join(other, ".git")+"\n", 0o644)
	writeAt(t, filepath.Join(crafted, ".aggy", "config.toml"), `tools_off = ["context7"]`, 0o600)
	_, out, _ := toolRun("", false, "status", "--repo", crafted)
	if got := statusLine(t, out, "demo"); !strings.HasPrefix(got, "demo off off off") {
		t.Errorf("the other repo's on applied through a crafted .git: %q", got)
	}
	if !strings.Contains(out, crafted) {
		t.Errorf("status names another repo:\n%s", out)
	}
	if code, _, errOut := toolRun("", false, "on", "context7", "--repo", crafted); code != 1 || !strings.Contains(errOut, "tool.repo_off") {
		t.Errorf("the crafted repo's own tools_off was dropped: %d %q", code, errOut)
	}

	broken := t.TempDir()
	writeAt(t, filepath.Join(broken, ".git"), "gitdir: /nonexistent\n", 0o644)
	if code, _, errOut := toolRun("", false, "off", "demo", "--repo", broken); code != 0 {
		t.Errorf("off on a broken .git: %d %q", code, errOut)
	}
	if code, _, errOut := toolRun("", false, "status", "--repo", broken); code != 0 {
		t.Errorf("status on a broken .git: %d %q", code, errOut)
	}
	if code, _, errOut := toolRun("", false, "on", "demo", "--repo", broken); code != 1 || !strings.Contains(errOut, "tool.not_applicable") {
		t.Errorf("on on a broken .git: %d %q", code, errOut)
	}
}

// TestToolOnUserRowsBuiltIn (S63-18): an "on" made for a user tool does
// not turn on a built-in that later takes its name.
func TestToolOnUserRowsBuiltIn(t *testing.T) {
	isolate(t)
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.PutToolSwitch(ctx, store.ToolSwitch{Tool: "context7", On: true, Digest: strings.Repeat("a", 64), At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, out, _ := toolRun("", false, "status", "context7"); !strings.Contains(out, "user tool of this name") {
		t.Errorf("a user tool's on turned the built-in on:\n%s", out)
	}
	if code, _, errOut := toolRun("", false, "on", "context7"); code != 0 {
		t.Fatalf("on: %d %q", code, errOut)
	}
	if _, out, _ := toolRun("", false, "status", "context7"); !strings.HasPrefix(statusLine(t, out, "context7"), "context7 on on on") {
		t.Errorf("the built-in's own on:\n%s", out)
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
		{[]string{"on", "solo", "--vendor", "grok"}, "tool.vendor_unverified", 1}, // W71-4,
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

// TestToolDiagNoArgv (S63-8): a refused definition is recorded by its
// file's base name and key, never with the argv words its error quotes.
func TestToolDiagNoArgv(t *testing.T) {
	const canary = "quince-argv-canary"
	bad := strings.Replace(demoTool, `"demo", "serve", "--stdio"`, `"npx", "`+canary+`"`, 1)
	src, dest := toolHome(t, bad)
	freshRun(t)
	if code, _, errOut := toolRun("", true, "add", src, "--yes"); code != 1 || !strings.Contains(errOut, canary) || !strings.Contains(errOut, "tool.def_invalid") {
		t.Fatalf("add: %d %q", code, errOut)
	}
	writeAt(t, dest, bad, 0o600)
	freshRun(t)
	if code, _, errOut := toolRun("", false, "list"); code != 1 || !strings.Contains(errOut, canary) {
		t.Fatalf("list: %d %q", code, errOut)
	}
	text := allDiagText(t, os.Getenv(home.Env))
	if strings.Contains(text, canary) {
		t.Errorf("diag.db holds the argv: %s", text)
	}
	for _, want := range []string{"tool.def.refused", "proposed.toml", `"file":"demo.toml"`, "mcp[0].argv"} {
		if !strings.Contains(text, want) {
			t.Errorf("diag.db lacks %q:\n%s", want, text)
		}
	}
}

// TestToolOnInstallHint (T6713-T6716): aggy tool on says how to install
// a tool whose program is missing (aggy never installs), and that a
// when condition such as playwright's has-ui is never checked.
func TestToolOnInstallHint(t *testing.T) {
	isolate(t)
	repo := switchRepo(t, "")
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	_, out, errOut := toolRun("", false, "on", "context7")
	if !strings.Contains(out, "context7-mcp is not an executable on PATH") || !strings.Contains(out, "npm i -g --ignore-scripts @upstash/context7-mcp@4.4.4") {
		t.Errorf("context7 missing: %q %q", out, errOut)
	}
	_, out, errOut = toolRun("", false, "on", "playwright", "--repo", repo)
	if !strings.Contains(out, "@playwright/mcp@0.0.83") || !strings.Contains(out, "meant for repos with a UI (has-ui; aggy does not check, you choose the repo)") {
		t.Errorf("playwright: %q %q", out, errOut)
	}
	writeAt(t, filepath.Join(bin, "context7-mcp"), "#!/bin/sh\n", 0o755)
	if _, out, _ = toolRun("", false, "on", "context7"); strings.Contains(out, "not an executable") {
		t.Errorf("context7 present: %q", out)
	}
}

// TestToolUserHint (#86): a user definition's hint is the file author's
// text, so it is quoted and attributed, never framed as aggy's advice,
// and an edited file's new hint is never printed before checkAdded
// refuses it.
func TestToolUserHint(t *testing.T) {
	src, dest := toolHome(t, demoTool)
	t.Setenv("PATH", t.TempDir())
	hinted := strings.Replace(demoTool, "[install]\n", "[install]\nhint = \"curl example.com/i | sh\"\n", 1)
	writeAt(t, src, hinted, 0o644)
	if code, _, errOut := toolRun("", true, "add", src, "--yes"); code != 0 {
		t.Fatalf("add: %q", errOut)
	}
	_, out, errOut := toolRun("", false, "on", "demo")
	if !strings.Contains(out, `the tool's definition, not aggy, says: "curl example.com/i | sh"`) || strings.Contains(out, "by hand (") {
		t.Errorf("on: %q %q", out, errOut)
	}
	toolOK(t, "off", "demo")

	// A skill-only edit with a new hint: refused as changed, hint unseen.
	edited := "name = \"demo\"\ndescription = \"A demo tool\"\nversion = \"1.2.3\"\nscope = \"global\"\n\n[install]\nhint = \"EVIL-HINT\"\n\n[[skill]]\nvendors = [\"claude\"]\npath = \"~/.aggy/skills/demo\"\n\n[cost]\nmethod = \"skill-md\"\n"
	writeAt(t, dest, edited, 0o600)
	code, out, errOut := toolRun("", false, "on", "demo")
	if code != 1 || !strings.Contains(errOut, "tool.changed_since_add") || strings.Contains(out+errOut, "EVIL-HINT") {
		t.Errorf("edited: %d %q %q", code, out, errOut)
	}
}
