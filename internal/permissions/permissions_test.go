package permissions

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

func TestCheckAllow(t *testing.T) {
	for _, tc := range []struct {
		rule string
		want Class
	}{
		{"Bash", Drop},
		{"Bash(*)", Drop},
		{"Bash( *)", Drop},
		{"Bash(**)", Drop},
		{"Bash(:*)", Drop},
		{"PowerShell", Drop},
		{"PowerShell(*)", Drop},
		{"PowerShell(Get-ChildItem *)", RepoCode},
		{"Agent", Drop},
		{"Agent(Explore)", Drop},
		{"Monitor", Drop},
		{"Bash(python*)", Drop},
		{"Bash(python3 *)", Drop},
		{"Bash(python3:*)", Drop},
		{"Bash(node *)", Drop},
		{"Bash(bash -c *)", Drop},
		{"Bash(* --help)", Drop},
		{"Bash(env *)", Drop},
		{"Bash(sudo *)", Drop},
		{"Bash(xargs *)", Drop},
		{"Bash(npm run *)", Drop},
		{"Bash(npm run build)", RepoCode},
		{"Bash(npm *)", Drop},
		{"Bash(npx *)", Drop},
		{"Bash(npx tsc)", RepoCode},
		{"Bash(pnpm exec foo)", RepoCode},
		{"Bash(uv run pytest)", RepoCode},
		{"Bash(go run .)", RepoCode},
		{"Bash(go *)", Drop},
		{"Bash(cargo *)", Drop},
		{"Bash(cargo run:*)", Drop},
		{"Bash(bundle exec rspec)", RepoCode},
		{"Bash(/usr/bin/python3 *)", Drop},
		{"Bash(FOO=1 python3 *)", Drop},
		{"Bash(/usr/local/bin/npm run build)", RepoCode},
		{"Bash(/bin/sh *)", Drop},
		{"Bash(/usr/bin/env *)", Drop},
		{"Bash(FOO=1 npm run x)", RepoCode},
		{"Bash(./node_modules/.bin/tsc)", RepoCode},
		{"Bash(/opt/x/tool *)", RepoCode},
		{"Bash(FOO=1 git status)", RepoCode},
		{"Bash(python3 tool.py)", RepoCode},
		{"Bash(npm test)", RepoCode},
		{"Bash(npm test *)", RepoCode},
		{"Bash(make build)", RepoCode},
		{"Bash(make *)", RepoCode},
		{"Bash(go test ./...)", RepoCode},
		{"Bash(./scripts/ci.sh)", RepoCode},
		{"Bash(bin/aggy *)", RepoCode},
		{"Bash(git status)", Keep},
		{"Bash(git diff *)", RepoCode},
		{"Bash(git diff --stat)", RepoCode},
		{"Bash(git diff)", RepoCode},
		{"Bash(git show)", RepoCode},
		{"Bash(git blame x)", RepoCode},
		{"Bash(git log)", RepoCode},
		{"Bash(git status *)", RepoCode},
		{"Bash(git status --short)", Keep},
		{"Bash(git diff --no-ext-diff --no-textconv)", Keep},
		{"Bash(git diff --no-ext-diff --no-textconv --stat)", Keep},
		{"Bash(git log --oneline -20)", Keep},
		{"Bash(git branch --show-current)", Keep},
		{"Bash(file x)", Keep},
		{"Bash(file -C -m x)", RepoCode},
		{"Bash(file -f x)", RepoCode},
		{"Bash(date)", Keep},
		{"Bash(date +%s)", Keep},
		{"Bash(date -s 12:00)", RepoCode},
		{"Bash(date 0101)", RepoCode},
		{"Bash(rg --hostname-bin=./evil x)", RepoCode},
		{"Bash(rg -z x)", RepoCode},
		{"Bash(noglob python *)", Drop},
		{"Bash(noglob python)", Drop},
		{"Bash(py\\thon *)", Drop},
		{"Bash(ipython *)", Drop},
		{"Read(~/**)", Drop},
		{"Read(~/.ssh/*)", Drop},
		{"Read(.ssh/id_rsa)", Drop},
		{"Edit(~/.aws/config)", Drop},
		{"Bash(cat ~/.ssh/id_rsa)", Drop},
		{"Bash(cat $HOME/.gnupg/x)", Drop},
		{"Read(*)", RepoCode},
		{"Read(**)", RepoCode},
		{"Read(**/*)", RepoCode},
		{"Read(*.md)", RepoCode},
		{"Read(../x)", RepoCode},
		{"Grep(*)", RepoCode},
		{"Glob(**)", RepoCode},
		{"WebFetch(https://*)", RepoCode},
		{"WebFetch(domain:*)", RepoCode},
		{"WebFetch(domain:*.com)", RepoCode},
		{"Read(docs/x.md)", Keep},
		{"Bash(git diff --output=x)", RepoCode},
		{"Bash(rg x)", Keep},
		{"Bash(rg *)", RepoCode},
		{"Bash(rg --pre x y)", RepoCode},
		{"Bash(ls *)", Keep},
		{"Bash(cat *)", Keep},
		{"Bash(grep *)", Keep},
		{"Bash(pwd)", Keep},
		{"Bash(gh pr view *)", RepoCode},
		{"Read", RepoCode},
		{"Read(./docs/**)", Keep},
		{"WebFetch(domain:example.com)", Keep},
		{"mcp__puppeteer__*", RepoCode},
		// An unknown program fails closed.
		{"Bash(prettier *)", RepoCode},
		{"Bash(rails *)", RepoCode},
		{"Bash(terraform *)", RepoCode},
		// The review's spellings: each is one of the classes above.
		{"Bash(python3.12 *)", Drop},
		{"Bash(python3.12 -c *)", Drop},
		{"Bash(node22 *)", Drop},
		{"Bash(perl5.34 *)", Drop},
		{"Bash(ruby3.3 *)", Drop},
		{"Bash(lua5.4 *)", Drop},
		{"Bash(php8.3 *)", Drop},
		{"Bash(julia *)", Drop},
		{"Bash(Rscript *)", Drop},
		{"Bash(busybox *)", Drop},
		{"Bash(ash *)", Drop},
		{"Bash(PYTHON *)", Drop},
		{"Bash(Python3 *)", Drop},
		{"Bash(\\python *)", Drop},
		{"Bash(\"python\" *)", Drop},
		{"Bash(python\t-c *)", Drop},
		{"Bash(sudo\t*)", Drop},
		{"Bash(MAKE *)", RepoCode},
		{"bash(*)", Drop},
		{"BASH(*)", Drop},
		{"Bash (*)", Drop},
		{"bash(ls)", RepoCode},
		{"*", Drop},
		{"mcp__*", Drop},
		{"Task", Drop},
		{"Task(*)", Drop},
		{"PowerShell(python *)", Drop},
		{"PowerShell(npm run *)", Drop},
		{"PowerShell(iex *)", Drop},
		{"PowerShell(make *)", RepoCode},
		{"PowerShell(git status)", RepoCode},
		{"Bash(devbox run *)", Drop},
		{"Bash(direnv exec *)", Drop},
		{"Bash(mise exec *)", Drop},
		{"Bash(docker exec *)", Drop},
		{"Bash(docker run *)", Drop},
		{"Bash(nix run *)", Drop},
		{"Bash(nix-shell *)", Drop},
		{"Bash(docker compose *)", RepoCode},
		{"Bash(git -c *)", Drop},
		{"Bash(git *)", Drop},
		{"Bash(git:*)", Drop},
		{"Bash(git --no-pager *)", Drop},
		{"Bash(git -c alias.x=!id x)", Drop},
		{"Bash(git --no-pager log)", RepoCode},
		{"Bash(setsid *)", Drop},
		{"Bash(flock *)", Drop},
		{"Bash(find *)", Drop},
		{"Bash(watch *)", Drop},
		{"Bash(ionice *)", Drop},
		{"Bash(caffeinate *)", Drop},
		{"Bash(chroot *)", Drop},
		{"Bash(tmux *)", Drop},
		{"Bash(vim *)", Drop},
		{"Bash(sed *)", Drop},
		{"Bash(npm --prefix . run *)", Drop},
		{"Bash(npm -w x exec *)", Drop},
		{"Bash(pnpm -r exec *)", Drop},
		{"Bash(yarn workspace x run *)", Drop},
		{"Bash(cargo --quiet run *)", Drop},
		{"Bash(npm ru*)", Drop},
		{"Bash(npm t*)", Drop},
		{"Bash(np* run *)", Drop},
		{"Bash(uv *)", Drop},
		{"Bash(sudo sh)", Drop},
		{"Bash(env python)", Drop},
		{"Bash(xargs sh)", Drop},
		{"Bash(sudo make)", RepoCode},
		{"Bash(?? *)", Drop},
		{"Bash(pyth?n *)", Drop},
		{"Bash(pyth[o]n *)", Drop},
		{"Bash(FOO=1 *)", Drop},
		{"Bash(FOO=$(x) python *)", Drop},
		{"--always-approve", Drop},
		{"-x", Drop},
		// A compound rule is never read-only: TODO(T6955) judges each part.
		{"Bash(ls && curl x)", RepoCode},
		{"Bash(ls | sh)", RepoCode},
		// A read-only form written any other way waits.
		{"Bash(/bin/ls)", RepoCode},
		{"Bash(GIT status)", RepoCode},
		{"Bash(", Drop},
		{" Bash(ls)", Drop},
		{"Bash(ls\n)", Drop},
	} {
		if got, why := CheckAllow(tc.rule); got != tc.want {
			t.Errorf("%q: class %d (%s), want %d", tc.rule, got, why, tc.want)
		}
	}
}

func TestCheckMode(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		bad      bool
	}{
		{"auto", "auto", false},
		{"manual", "default", false},
		{"plan", "plan", false},
		{"bypassPermissions", "", true},
		{"yolo", "", true},
		{"", "", true},
	} {
		got, err := CheckMode(tc.in)
		if got != tc.want || (err != nil) != tc.bad {
			t.Errorf("%q: %q, %v", tc.in, got, err)
		}
	}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fixture is a Claude folder, a state file, and a repo.
type fixture struct{ in Input }

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	main := filepath.Join(dir, "repo")
	if err := os.MkdirAll(main, 0o700); err != nil {
		t.Fatal(err)
	}
	return fixture{Input{
		ClaudeDir: filepath.Join(dir, ".claude"),
		AggyFile:  filepath.Join(dir, ".aggy", FileName),
		Main:      main, RepoKey: main,
		Tracked: func(string) (bool, error) { return false, nil },
	}}
}

func (f fixture) local() string { return filepath.Join(f.in.Main, ".claude", "settings.local.json") }

func texts(rules []Rule, k Kind) []string {
	var out []string
	for _, r := range rules {
		if r.Kind == k {
			out = append(out, r.Text+"@"+string(r.Origin))
		}
	}
	return out
}

func hasSkip(s Set, text string) bool {
	for _, k := range s.Skipped {
		if k.Text == text {
			return true
		}
	}
	return false
}

func TestLoadSources(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.in.ClaudeDir, "settings.json"), `{"permissions":{"defaultMode":"auto","allow":["Bash(git status)","Bash(*)"],"deny":["Read(./x)"]},"model":"m"}`)
	write(t, f.local(), `{"permissions":{"allow":["Bash(git diff *)","Bash(npm test)"],"ask":["Bash(git push *)"]}}`)
	write(t, f.in.AggyFile, `{"permissions":{"allow":["Bash(ls)"]},"repos":{"`+f.in.Main+`":{"allow":["Bash(pwd)"]},"/other":{"allow":["Bash(whoami)"]}}}`)
	s := Load(f.in)
	if got := strings.Join(texts(s.Rules, Allow), " "); got != "Bash(git status)@claude-user Bash(ls)@aggy Bash(pwd)@aggy-repo" {
		t.Errorf("allow: %s", got)
	}
	if s.HeldLocal != 2 {
		t.Errorf("a repo's local allows wait for trust: %d held", s.HeldLocal)
	}
	if got := strings.Join(texts(s.Rules, Ask), " "); got != "Bash(git push *)@claude-local" {
		t.Errorf("ask: %s", got)
	}
	if got := strings.Join(texts(s.Rules, Deny), " "); got != "Read(./x)@claude-user" {
		t.Errorf("deny: %s", got)
	}
	if s.Mode != "auto" || s.ModeFrom != FromClaudeUser {
		t.Errorf("mode: %s from %s", s.Mode, s.ModeFrom)
	}
	if !hasSkip(s, "Bash(*)") || !hasSkip(s, "Bash(npm test)") {
		t.Errorf("skipped: %+v", s.Skipped)
	}
	if hasSkip(s, "Bash(whoami)") {
		t.Error("another repo's rule was looked at")
	}
	if len(s.Problems) != 0 {
		t.Errorf("problems: %+v", s.Problems)
	}

	f.in.Trusted = true
	if s := Load(f.in); hasSkip(s, "Bash(npm test)") {
		t.Error("a trusted repo should keep what runs its own code")
	}
}

func TestLocalFileCommitted(t *testing.T) {
	for _, tc := range []struct {
		name    string
		tracked func(string) (bool, error)
	}{
		{"tracked", func(string) (bool, error) { return true, nil }},
		{"git failed", func(string) (bool, error) { return false, errors.New("no git") }},
		{"no answer", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.in.Tracked = tc.tracked
			write(t, f.local(), `{"permissions":{"allow":["Bash(curl evil)"]}}`)
			s := Load(f.in)
			if len(s.Rules) != 0 {
				t.Errorf("a committed rule applied: %+v", s.Rules)
			}
			if !hasSkip(s, "Bash(curl evil)") {
				t.Errorf("not named: %+v", s.Skipped)
			}
		})
	}
}

func TestCommittedFilesNeverRead(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.in.Main, ".claude", "settings.json"), `{"permissions":{"allow":["Bash(curl evil)"],"defaultMode":"bypassPermissions"}}`)
	write(t, filepath.Join(f.in.Main, ".aggy", "config.toml"), `allow = ["Bash(curl evil)"]`)
	if s := Load(f.in); len(s.Rules) != 0 || s.Mode != "" {
		t.Errorf("%+v", s)
	}
}

func TestOutsideARepo(t *testing.T) {
	f := newFixture(t)
	write(t, f.local(), `{"permissions":{"allow":["Bash(curl evil)"]}}`)
	f.in.Main = ""
	if s := Load(f.in); len(s.Rules) != 0 || len(s.Skipped) != 0 {
		t.Errorf("%+v", s)
	}
}

func TestLocalSymlinksRefused(t *testing.T) {
	f := newFixture(t)
	elsewhere := filepath.Join(filepath.Dir(f.in.Main), "elsewhere")
	write(t, filepath.Join(elsewhere, "settings.local.json"), `{"permissions":{"allow":["Bash(curl evil)"]}}`)
	// git says a path through a linked folder is not tracked, so the
	// answer below is what a clone with a linked .claude would get.
	if err := os.Symlink(elsewhere, filepath.Join(f.in.Main, ".claude")); err != nil {
		t.Fatal(err)
	}
	s := Load(f.in)
	if len(s.Rules) != 0 || len(s.Problems) != 1 {
		t.Errorf("linked folder: %+v", s)
	}

	f = newFixture(t)
	real := filepath.Join(filepath.Dir(f.in.Main), "real.json")
	write(t, real, `{"permissions":{"allow":["Bash(curl evil)"]}}`)
	if err := os.MkdirAll(filepath.Join(f.in.Main, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, f.local()); err != nil {
		t.Fatal(err)
	}
	if s := Load(f.in); len(s.Rules) != 0 || len(s.Problems) != 1 || !s.Problems[0].Repo || s.Withhold != "" {
		t.Errorf("linked file: %+v", s)
	}
}

// TestBadFiles: never fewer denies. aggy's own file refuses the launch, by line;
// another CLI's file withholds this layer's allows, and the launch goes on.
func TestBadFiles(t *testing.T) {
	const deny = `"deny":["Bash(git push *)"]`
	for _, tc := range []struct {
		name, body string
		line       int
		notes      int
		bad        bool
	}{
		{"trailing comma", "{\"permissions\":{" + deny + "},\n}", 2, 0, true},
		{"wrong type", `{"permissions":{"deny":"Bash(git push *)"}}`, 1, 0, true},
		{"text after", `{"permissions":{` + deny + `}} garbage`, 1, 0, true},
		{"a bad deny", `{"permissions":{"deny":[" bad"]}}`, 0, 0, true},
		{"a bad repo ask", `{"repos":{"/x":{"ask":["-x"]}},"permissions":{"ask":["-x"]}}`, 0, 0, true},
		{"$schema", `{"$schema":"https://x","permissions":{` + deny + `}}`, 0, 0, false},
		{"additionalDirectories", `{"permissions":{` + deny + `,"additionalDirectories":["/x"]}}`, 0, 1, false},
		{"a key at the top", `{"future":1,"permissions":{` + deny + `}}`, 0, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			write(t, f.in.AggyFile, tc.body)
			s := Load(f.in)
			if tc.bad {
				if s.FileErr == nil || s.FileErr.Line != tc.line || !strings.Contains(s.FileErr.Error(), f.in.AggyFile) {
					t.Fatalf("file error: %+v", s.FileErr)
				}
				return
			}
			if s.FileErr != nil || s.Withhold != "" || len(s.Notes) != tc.notes {
				t.Errorf("%+v", s)
			}
			if got := texts(s.Rules, Deny); len(got) != 1 {
				t.Errorf("the deny went missing: %q", got)
			}
		})
	}

	for _, body := range []string{`{not json`, `{"permissions":{"deny":3}}`, `{"permissions":{"deny":[" bad"]}}`} {
		f := newFixture(t)
		f.in.Trusted = true
		write(t, filepath.Join(f.in.ClaudeDir, "settings.json"), body)
		write(t, f.in.AggyFile, `{"permissions":{"allow":["Bash(ls)"],"deny":["Bash(x y)"]}}`)
		s := Load(f.in)
		if s.FileErr != nil || len(s.Problems) != 1 || s.Withhold == "" {
			t.Fatalf("%s: %+v", body, s)
		}
		for _, v := range []adapter.Vendor{adapter.Claude, adapter.Grok} {
			r := s.Render(v)
			if len(r.Allow) != 0 || !slices.Contains(r.Deny, "Bash(x y)") || !slices.ContainsFunc(r.Skipped, func(k Skip) bool { return k.Why == s.Withhold }) {
				t.Errorf("%s: %s: %+v", body, v, r)
			}
		}
	}

	f := newFixture(t)
	write(t, f.in.AggyFile, `{"permissions":{"defaultMode":"bypassPermissions","deny":["Bash(rm x)"]}}`)
	if s := Load(f.in); s.Mode != "" || s.FileErr != nil || len(s.Notes) != 1 || len(s.Rules) != 1 {
		t.Errorf("%+v", s)
	}
}

// TestLocalAllowsWaitForTrust: an archive with a planted .git, or a name git and
// the file system disagree on, makes a committed file look like the person's.
func TestLocalAllowsWaitForTrust(t *testing.T) {
	f := newFixture(t)
	write(t, f.local(), `{"permissions":{"allow":["Bash(git status)","Bash(make x)"],"deny":["Bash(x y)"]}}`)
	s := Load(f.in)
	if got := texts(s.Rules, Allow); len(got) != 0 || s.HeldLocal != 2 {
		t.Errorf("untrusted: %q, held %d", got, s.HeldLocal)
	}
	if got := texts(s.Rules, Deny); len(got) != 1 {
		t.Errorf("a deny narrows, so it stays: %q", got)
	}
	f.in.Trusted = true
	s = Load(f.in)
	if got := texts(s.Rules, Allow); len(got) != 2 || s.HeldLocal != 0 {
		t.Errorf("trusted: %q, held %d", got, s.HeldLocal)
	}
}

// TestLocalFileWhoseProblem: a clone brings only a tracked file or a link, and
// those must leave the person's allows alone. The person's own untracked file keeps
// every valid deny and ask, and a mistake in it withholds, as a deny may be missing.
func TestLocalFileWhoseProblem(t *testing.T) {
	notTracked := func(string) (bool, error) { return false, nil }
	tracked := func(string) (bool, error) { return true, nil }
	for _, tc := range []struct {
		name    string
		body    string
		tracked func(string) (bool, error)
		link    string // "file" or "dir"
		repo    bool
		denies  []string
		asks    []string
	}{
		{name: "trailing comma", body: `{"permissions":{"deny":["Bash(git push x)"],}}`, tracked: notTracked},
		{name: "typo in a deny", body: `{"permissions":{"deny":["Bash(git push x)","Bash("],"ask":["Bash(npm publish)"]}}`, tracked: notTracked,
			denies: []string{"Bash(git push x)@claude-local"}, asks: []string{"Bash(npm publish)@claude-local"}},
		{name: "typo in an ask", body: `{"permissions":{"deny":["Bash(git push x)"],"ask":["Bash("]}}`, tracked: notTracked,
			denies: []string{"Bash(git push x)@claude-local"}},
		{name: "tracked, bad JSON", body: "{", tracked: tracked, repo: true},
		{name: "git cannot say, bad JSON", body: "{", repo: true},
		{name: "linked file", body: `{"permissions":{"deny":["Bash(git push x)"]}}`, tracked: notTracked, link: "file", repo: true},
		{name: "linked folder", body: `{"permissions":{"deny":["Bash(git push x)"]}}`, tracked: notTracked, link: "dir", repo: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.in.Trusted = true
			f.in.Tracked = tc.tracked
			write(t, f.in.AggyFile, `{"permissions":{"allow":["Bash(make x)"]}}`)
			elsewhere := filepath.Join(filepath.Dir(f.in.Main), "elsewhere")
			switch tc.link {
			case "":
				write(t, f.local(), tc.body)
			case "file":
				write(t, filepath.Join(elsewhere, "real.json"), tc.body)
				if err := os.MkdirAll(filepath.Dir(f.local()), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(elsewhere, "real.json"), f.local()); err != nil {
					t.Fatal(err)
				}
			case "dir":
				write(t, filepath.Join(elsewhere, "settings.local.json"), tc.body)
				if err := os.Symlink(elsewhere, filepath.Dir(f.local())); err != nil {
					t.Fatal(err)
				}
			}
			s := Load(f.in)
			if len(s.Problems) != 1 || s.Problems[0].Repo != tc.repo {
				t.Fatalf("problems: %+v", s.Problems)
			}
			if got := texts(s.Rules, Deny); !slices.Equal(got, tc.denies) {
				t.Errorf("denies: %q", got)
			}
			if got := texts(s.Rules, Ask); !slices.Equal(got, tc.asks) {
				t.Errorf("asks: %q", got)
			}
			r := s.Render(adapter.Claude)
			if tc.repo {
				if s.Withhold != "" || !slices.Equal(r.Allow, []string{"Bash(make x)"}) {
					t.Errorf("a repo turned off the person's allows: %q, %q", s.Withhold, r.Allow)
				}
				return
			}
			if !strings.Contains(s.Withhold, f.local()) || !strings.Contains(s.Withhold, "a deny may be missing") || len(r.Allow) != 0 {
				t.Errorf("withhold %q, allow %q", s.Withhold, r.Allow)
			}
		})
	}
}

// TestLinkToCredentialIsTheRepos: safefs refuses it by path, and a clone can still
// bring the link, so it must not turn off the person's allows either.
func TestLinkToCredentialIsTheRepos(t *testing.T) {
	f := newFixture(t)
	home := filepath.Dir(f.in.Main)
	write(t, filepath.Join(home, ".codex", "auth.json"), "{}")
	if err := os.MkdirAll(filepath.Dir(f.local()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".codex", "auth.json"), f.local()); err != nil {
		t.Fatal(err)
	}
	if s := Load(f.in); s.Withhold != "" || len(s.Problems) != 1 || !s.Problems[0].Repo {
		t.Errorf("%+v", s)
	}
}

// TestNoClaudeFolder: with HOME unset an empty folder must not join into a relative
// path, or the repo's committed settings.json is read as the person's.
func TestNoClaudeFolder(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.in.Main, "settings.json"), `{"permissions":{"allow":["Bash(ls)"],"defaultMode":"auto"}}`)
	t.Chdir(f.in.Main)
	f.in.ClaudeDir = ""
	s := Load(f.in)
	if len(s.Rules) != 0 || s.Mode != "" {
		t.Errorf("read the repo's file as the person's: %+v", s)
	}
	if len(s.Problems) != 1 || s.Problems[0].Repo || s.Withhold == "" {
		t.Errorf("%+v", s)
	}
}

// TestLinkedClaudeSettings: stow and chezmoi link the file, or the folder.
func TestLinkedClaudeSettings(t *testing.T) {
	f := newFixture(t)
	dot := filepath.Join(filepath.Dir(f.in.Main), "dotfiles", "settings.json")
	write(t, dot, `{"permissions":{"allow":["Bash(ls)"],"deny":["Bash(x y)"]}}`)
	if err := os.MkdirAll(f.in.ClaudeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dot, filepath.Join(f.in.ClaudeDir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	s := Load(f.in)
	if len(s.Problems) != 0 || s.Withhold != "" || len(texts(s.Rules, Deny)) != 1 {
		t.Errorf("linked file: %+v", s)
	}

	f = newFixture(t)
	dir := filepath.Join(filepath.Dir(f.in.Main), "dotclaude")
	write(t, filepath.Join(dir, "settings.json"), `{"permissions":{"deny":["Bash(x y)"]}}`)
	if err := os.Symlink(dir, f.in.ClaudeDir); err != nil {
		t.Fatal(err)
	}
	if s := Load(f.in); len(s.Problems) != 0 || len(texts(s.Rules, Deny)) != 1 {
		t.Errorf("linked folder: %+v", s)
	}

	// A link to a credential file is refused, and counts as unreadable.
	f = newFixture(t)
	home := filepath.Dir(f.in.Main)
	write(t, filepath.Join(home, ".claude", ".credentials.json"), `{}`)
	if err := os.Symlink(filepath.Join(home, ".claude", ".credentials.json"), filepath.Join(home, ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	if s := Load(f.in); len(s.Problems) != 1 || s.Withhold == "" {
		t.Errorf("credential link: %+v", s)
	}
}
