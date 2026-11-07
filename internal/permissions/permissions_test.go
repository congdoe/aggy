package permissions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
		{"PowerShell(Get-ChildItem *)", Keep},
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
		{"Bash(npm run build)", Drop},
		{"Bash(npm *)", Drop},
		{"Bash(npx *)", Drop},
		{"Bash(npx tsc)", Drop},
		{"Bash(pnpm exec foo)", Drop},
		{"Bash(uv run pytest)", Drop},
		{"Bash(go run .)", Drop},
		{"Bash(go *)", Drop},
		{"Bash(cargo *)", Drop},
		{"Bash(cargo run:*)", Drop},
		{"Bash(bundle exec rspec)", Drop},
		{"Bash(python3 tool.py)", RepoCode},
		{"Bash(npm test)", RepoCode},
		{"Bash(npm test *)", RepoCode},
		{"Bash(make build)", RepoCode},
		{"Bash(make *)", RepoCode},
		{"Bash(go test ./...)", RepoCode},
		{"Bash(./scripts/ci.sh)", RepoCode},
		{"Bash(bin/aggy *)", RepoCode},
		{"Bash(git status)", Keep},
		{"Bash(git diff *)", Keep},
		{"Bash(gh pr view *)", Keep},
		{"Read", Keep},
		{"Read(./docs/**)", Keep},
		{"WebFetch(domain:example.com)", Keep},
		{"mcp__puppeteer__*", Keep},
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
	if got := strings.Join(texts(s.Rules, Allow), " "); got != "Bash(git status)@claude-user Bash(git diff *)@claude-local Bash(ls)@aggy Bash(pwd)@aggy-repo" {
		t.Errorf("allow: %s", got)
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
	if s := Load(f.in); len(s.Rules) != 0 || len(s.Problems) != 1 {
		t.Errorf("linked file: %+v", s)
	}
}

func TestBadFilesAreProblems(t *testing.T) {
	f := newFixture(t)
	write(t, f.in.AggyFile, `{"permissions":{"alow":["Bash(ls)"]}}`)
	write(t, filepath.Join(f.in.ClaudeDir, "settings.json"), `{not json`)
	s := Load(f.in)
	if len(s.Rules) != 0 || len(s.Problems) != 2 {
		t.Errorf("%+v", s)
	}
	f = newFixture(t)
	write(t, f.in.AggyFile, `{"permissions":{"defaultMode":"bypassPermissions","deny":["Bash(rm x)"," bad"]}}`)
	s = Load(f.in)
	if s.Mode != "" || len(s.Problems) != 1 || !hasSkip(s, " bad") || len(s.Rules) != 1 {
		t.Errorf("%+v", s)
	}
}
