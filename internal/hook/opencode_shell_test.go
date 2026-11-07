package hook

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// plantsGit are shapes that change which git a following `git status` runs,
// with nothing else for OpenCode to ask about in most of them (scan.ts at
// v2.2.5 gave them only `git status`; see docs/verification.md). Each uses
// ./bin, where the test plants a git.
var plantsGit = []string{
	"PATH=./bin; git status",
	"PATH=./bin\ngit status",
	"PATH=./bin && git status",
	"export PATH=./bin; git status",
	"typeset -x PATH=./bin; git status",
	"(PATH=./bin; git status)",
	"{ PATH=./bin; git status; }",
	"$(PATH=./bin; git status)",
	"if true; then PATH=./bin; fi; git status",
	"case x in x) PATH=./bin;; esac; git status",
	"while PATH=./bin; do git status; break; done",
	"until PATH=./bin; do git status; break; done",
	"for PATH in ./bin; do git status; done",
	"for PATH in ./bin\ndo git status; done",
	"select PATH in ./bin; do git status; break; done",
	"for x in \"$(PATH=./bin; git status)\"; do :; done",
	"[[ -n \"$(PATH=./bin; git status)\" ]]",
	"cat <<EOF\n$(PATH=./bin; git status)\nEOF",
	"cat <<EOF; PATH=./bin; git status\nbody\nEOF",
	"[[ ${#} ]]; PATH=./bin; git status",
	"for i in ${#a}; do :; done; PATH=./bin; git status",
	"for i in $((1<<2)); do :; done\nPATH=./bin\ngit status",
	"(( 1<<2 ))\nPATH=./bin\ngit status",
	"cat <<\"E F\"\nhi\nE F\nPATH=./bin\ngit status",
	"[[ $'\\'' ]]; PATH=./bin; git status",
	"cat <<EOF\nEOF\nPATH=./bin; git status",
	"PATH[0]=./bin; git status",
	"path=(./bin); git status",
	"path+=(./bin); git status",
	"path[1]=./bin; git status",
	"for path in ./bin; do git status; done",
	"for path (./bin) git status",
	"git() { ./bin/git; }; git status",
	"git ( ) { ./bin/git; }; git status",
	"git\t()\t{ ./bin/git; }; git status",
	"git()\n{ ./bin/git; }\ngit status",
	"\"git\"() { ./bin/git; }; git status",
	"\\git() { ./bin/git; }; git status",
	"function git { ./bin/git; }; git status",
	"function git() { ./bin/git; }; git status",
	"function -T git { ./bin/git; }; git status",
	"git foo() { ./bin/git; }; git status",
	"function foo git { ./bin/git; }; git status",
	"PA\\\nTH=./bin; git status",
	"! PATH=./bin; git status",
	"true; PATH=./bin; git status",
	"command export PATH=./bin; git status",
	"builtin export PATH=./bin; git status",
	"FOO=1 export PATH=./bin; git status",
	"repeat 1 PATH=./bin; git status",
	"# note\\\nPATH=./bin:$PATH\ngit status",
	"git status # x\\\nPATH=./bin\ngit status",
	"#\\\nPATH=./bin\ngit status",
	"time -p for PATH in ./bin; do git status; done",
	"time -p if PATH=./bin; then git status; fi",
	"! time -p for PATH in ./bin; do git status; done",
	"time -p while PATH=./bin; git status; do break; done",
	"PATH[x[0]]=./bin; git status",
	">x PATH=./bin; git status",
	"eval PATH=./bin; git status",
	"read PATH <<<./bin; git status",
	"printf -v PATH ./bin; git status",
	"declare -n r=PATH; r=./bin; git status",
	"X=PATH; export \"$X=./bin\"; git status",
	"X=PA; export \"${X}TH=./bin\"; git status",
	"Y=./bin; X=PA$'\\x54'H; declare -x \"$X=$Y\"; git status",
	": ${PATH:=./bin}; unset x; git status",
	"export P\"A\"TH=./bin; git status",
	"export PA\\TH=./bin; git status",
	"export 'PA'TH=./bin; git status",
	"export PA\\\n\"T\"H=./bin; git status",
	"eval \"PA\"\"TH=./bin\"; git status",
}

// settingDirectives are other shapes the scan must refuse. They do not plant a
// git, or not through PATH: they set a listed variable, or use a spelling only
// the raw text shows.
var settingDirectives = []string{
	"(( GIT_CONFIG_COUNT = 0 )); git status",
	"((PATH=1)); git status",
	"unset PATH; git status",
	"echo $((PATH=1)); git status",
	"echo ${PATH:=./bin}; git status",
	"echo ${PATH=./bin}; git status",
	"echo ${PATH::=./bin}; git status",
	"echo ${PATH[0]:=./bin}; git status",
	"for GIT_DIR in x; do git status; done",
	"GIT_CONFIG_VALUE_0=./evil; git status",
	"LD_PRELOAD=./x.so git status",
	"export DYLD_INSERT_LIBRARIES=./x.dylib",
	"BASH_ENV=./x; git status",
	"export 'BASH_FUNC_git%%=() { ./bin/git; }'",
	"ENV=./x; git status",
	"ZDOTDIR=./x; git status",
	"HOME=./x; git status",
	"XDG_CONFIG_HOME=./x; git status",
	"CDPATH=./x; git status",
	"SHELL=./x; git status",
	"PAGER=./x; git log --oneline -20",
	"MANPAGER=./x; git status",
	"EDITOR=./x; git status",
	"VISUAL=./x; git status",
	"RIPGREP_CONFIG_PATH=./x; git status",
	"cdpath=(./x); git status",
	"fpath=(./x); git status",
	"manpath=(./x); git status",
	"read path; git status",
	"read -r path; git status",
	"select path in ./bin; do git status; break; done",
	"foreach path (./bin) git status",
	"echo ${path:=./bin}; git status",
	"ls() { ./bin/git; }; ls",
	"function pwd { ./bin/git; }; pwd",
	"declare $X=1",
	"typeset \"$X=1\"",
	"export -- $X=1",
	"export A=1 $X=2",
	"readonly `echo X`=1",
	"local \"$X\"",
	"integer $X=1",
	"float $X=1",
	"set -- PATH=./bin; export \"$@\"",
}

var guardRuleWords = []string{"git", "ls", "pwd"}

// guardRefuses is the guard's answer on one shell command.
func guardRefuses(t *testing.T, root, cmd string) bool {
	t.Helper()
	b, err := json.Marshal(OpenCodeInput{Tool: "shell", Directory: root, Root: root, Command: cmd})
	if err != nil {
		t.Fatal(err)
	}
	return OpenCodeGuard(b, OpenCodeEnv{RuleWords: guardRuleWords, Getenv: func(string) string { return "" }}).Deny
}

// TestGuardShapes holds both sides of the scan: every shape two rounds of
// review found is refused, and what a session runs all day is not.
func TestGuardShapes(t *testing.T) {
	root := resolved(t)
	for _, list := range []struct {
		name   string
		shapes []string
		want   bool
	}{
		{"plants a git", plantsGit, true},
		{"sets a listed variable", settingDirectives, true},
		{"a review's bypass", reviewBypasses, true},
		{"everyday, but names a listed variable", everydayRefused, true},
		{"everyday", everydayCommands, false},
		{"passes by design", reviewDesignPasses, false},
	} {
		for _, c := range list.shapes {
			if got := guardRefuses(t, root, c); got != list.want {
				t.Errorf("%s: %q: refused = %v, want %v", list.name, c, got, list.want)
			}
		}
	}
}

// TestGuardNames: the refusal names what to rewrite.
func TestGuardNames(t *testing.T) {
	cases := []struct{ cmd, want string }{
		{"PATH=./bin; git status", "names PATH"},
		{"true; GIT_DIR=x", "names GIT_DIR"},
		{"path=(x)", "names path"},
		{"for fpath in x; do :; done", "names fpath"},
		{"echo $PATH; HOME=x", "names HOME"},
		{"git() { :; }", "named git"},
		{"function ls { :; }", "named ls"},
		{"export \"$X=1\"", "from a $ expansion"},
	}
	for _, c := range cases {
		if why := shellScan(c.cmd, guardRuleWords); !strings.Contains(why, c.want) {
			t.Errorf("%q: %q, want %q", c.cmd, why, c.want)
		}
	}
}

// TestGuardFunctionNamesComeFromTheRules: the names come from the rules rendered
// (parity.OpenCodeRuleWords), so another function is nobody's business.
func TestGuardFunctionNamesComeFromTheRules(t *testing.T) {
	if why := shellScan("make() { :; }", guardRuleWords); why != "" {
		t.Errorf("no rule starts with make: %q", why)
	}
	if why := shellScan("make() { :; }", []string{"make"}); why == "" {
		t.Error("a rule starting with make should count")
	}
}

// TestGuardQuoteRemovalCosts pins the false refusals docs/design.md says that
// form brings: the raw text alone passes these.
func TestGuardQuoteRemovalCosts(t *testing.T) {
	for _, c := range []string{
		"rg 'git\\(\\)' src",
		"echo \"export \\$FOO=1\"",
	} {
		if scanText(c, guardRuleWords) != "" {
			t.Errorf("%q: the raw text should pass", c)
		}
		if shellScan(c, guardRuleWords) == "" {
			t.Errorf("%q: passed; correct docs/design.md", c)
		}
	}
}

// TestGuardDocumentedGaps pins what docs/design.md says the scan does not
// catch, so a change to either side shows.
func TestGuardDocumentedGaps(t *testing.T) {
	for _, c := range []string{
		"FOO=1; export FOO; git status",     // a variable that is not on the list
		"eval \"${X}TH=./bin\"; git status", // built at run time, and eval asks
		"git foo() ./bin/git; git status",   // zsh; OpenCode's own scanner fails this call
	} {
		if why := shellScan(c, guardRuleWords); why != "" {
			t.Errorf("%q: refused (%q); correct docs/design.md", c, why)
		}
	}
}

// resolved is a temp folder by its real path.
func resolved(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestGuardAgainstRealShells runs every shape through each real shell here,
// with a planted git. One that runs the planted git must be refused, and one
// the guard lets through must not run it, in any shell. The shells are not
// vendor CLIs.
func TestGuardAgainstRealShells(t *testing.T) {
	type shell struct {
		path string
		args []string
	}
	var shells []shell
	for _, c := range []shell{{"/bin/bash", []string{"--noprofile", "--norc", "-c"}}, {"/bin/zsh", []string{"-f", "-c"}}, {"/bin/sh", []string{"-c"}}, {"/bin/dash", []string{"-c"}}} {
		if _, err := os.Stat(c.path); err == nil {
			shells = append(shells, c)
		}
	}
	if len(shells) == 0 {
		t.Skip("no shell here")
	}
	dir := resolved(t)
	home := filepath.Join(dir, "home")
	bin := filepath.Join(dir, "bin")
	for _, d := range []string{home, bin} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	marker := filepath.Join(dir, "planted-ran")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\n: > "+marker+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ranIn := func(sh shell, cmd string) bool {
		_ = os.Remove(marker)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c := exec.CommandContext(ctx, sh.path, append(append([]string{}, sh.args...), cmd)...)
		c.Dir = dir
		c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "GIT_CEILING_DIRECTORIES=" + dir}
		_ = c.Run() // a shape may be a syntax error in this shell
		_, err := os.Stat(marker)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		return err == nil
	}
	ranSomewhere := 0
	// What the guard allows must not run the planted git, in any shell. This
	// is everything from the reviews that passes by design, and the plain forms.
	// `IFS=_; git_status` would run it if a shell split literal text.
	allowed := append([]string{"git status", "git commit -m \"see ./bin\"", "for f in ./bin; do git status; done", "cat <<'EOF'\ntext\nEOF\ngit status", "FOO=./bin; git status", "IFS=_; git_status", "IFS=t; git status"}, reviewDesignPasses...)
	shapes := slices.Concat(plantsGit, reviewBypasses, allowed)
	for _, cmd := range shapes {
		for _, sh := range shells {
			if ranIn(sh, cmd) {
				ranSomewhere++
				if !guardRefuses(t, dir, cmd) {
					t.Errorf("%s ran the planted git on %q, and the guard allowed it", sh.path, cmd)
				}
			}
		}
	}
	// The harness itself must be able to see a planted git run.
	if ranSomewhere < len(plantsGit)/2 {
		t.Errorf("only %d shape and shell pairs ran the planted git: the test proves little", ranSomewhere)
	}
}
