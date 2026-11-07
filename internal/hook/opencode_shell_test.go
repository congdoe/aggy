package hook

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// guardRefuses is the guard's answer on one shell command.
func guardRefuses(t *testing.T, root, cmd string) bool {
	t.Helper()
	b, err := json.Marshal(OpenCodeInput{Tool: "shell", Directory: root, Root: root, Command: cmd})
	if err != nil {
		t.Fatal(err)
	}
	return OpenCodeGuard(b, OpenCodeEnv{Getenv: func(string) string { return "" }}).Deny
}

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
	// The old parser lost its place on each of these, and missed what followed.
	"[[ ${#} ]]; PATH=./bin; git status",
	"for i in ${#a}; do :; done; PATH=./bin; git status",
	"for i in $((1<<2)); do :; done\nPATH=./bin\ngit status",
	"(( 1<<2 ))\nPATH=./bin\ngit status",
	"cat <<\"E F\"\nhi\nE F\nPATH=./bin\ngit status",
	"[[ $'\\'' ]]; PATH=./bin; git status",
	"cat <<EOF\nEOF\nPATH=./bin; git status",
	"PATH[0]=./bin; git status",
	"path=(./bin); git status",
	"for path in ./bin; do git status; done",
	"git() { ./bin/git; }; git status",
	"function git { ./bin/git; }; git status",
	"PA\\\nTH=./bin; git status",
	"! PATH=./bin; git status",
	"true; PATH=./bin; git status",
	"command export PATH=./bin; git status",
	"builtin export PATH=./bin; git status",
	"FOO=1 export PATH=./bin; git status",
	"repeat 1 PATH=./bin; git status",
}

// TestGuardRefusesWhatSetsVariables has each shape H1 and H2 found, and others
// that OpenCode's scanner gives no resource for.
func TestGuardRefusesWhatSetsVariables(t *testing.T) {
	root := resolved(t)
	shapes := append([]string{
		"(( GIT_CONFIG_COUNT = 0 )); git status",
		"((PATH=1)); git status",
		"(( i++ )); git status",
		"for ((i=0;i<1;i++)); do git status; done",
		"FOO=(a b); git status",
		"unset PATH; git status",
		"declare FOO=1; git status",
		"readonly FOO=1; git status",
		"local FOO=1",
		"export\tFOO=1; git status",
		"echo $((PATH=1)); git status",
		"echo ${PATH:=./bin}; git status",
		"for no_proxy in x; do git status; done",
		"for GIT_DIR in x; do git status; done",
		"a=1 b=2",
	}, plantsGit...)
	for _, c := range shapes {
		if !guardRefuses(t, root, c) {
			t.Errorf("allowed %q", c)
		}
	}
}

// TestGuardAllowsEverydayCommands holds the other side: what a session runs
// all day must not stop.
func TestGuardAllowsEverydayCommands(t *testing.T) {
	root := resolved(t)
	for _, c := range []string{
		"git status",
		"git commit -m \"a=b\"",
		"git commit -m 'export a=b; c=d'",
		"git commit -m \"fix foo() in the for loop\"",
		"git log --format='%H' -- 'a;b=1'",
		"go test -run=TestX ./...",
		"VAR=1 go test ./...",
		"echo a=b",
		"echo hi > out.txt",
		"for f in *.go; do gofmt -l \"$f\"; done",
		"for f in a b\ndo echo \"$f\"; done",
		"while read f; do echo \"$f\"; done < list",
		"if [ -f x ]; then echo y; fi",
		"case x in x) echo hi;; esac",
		"[[ -f go.mod ]] && go test ./...",
		"[[ $a == b ]] && echo y",
		"(( a == 1 )) && echo y",
		"(( a <= 1 )) && echo y",
		"echo ${HOME}",
		"echo $((1+2))",
		"echo \"$(git rev-parse HEAD)\"",
		"git log --format=%H $(git merge-base a b)..HEAD",
		"cat <<EOF\nhello\nEOF",
		"cat <<'EOF'\nsome text, no equals\nEOF",
		"cd sub && make",
		"ls -la | head -5",
		"(cd sub && make)",
	} {
		if guardRefuses(t, root, c) {
			t.Errorf("refused %q", c)
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
	allowed := []string{"git status", "git commit -m \"PATH=./bin\"", "for f in ./bin; do git status; done", "echo PATH=./bin; git status", "cat <<'EOF'\ntext\nEOF\ngit status"}
	for _, cmd := range append(append([]string{}, plantsGit...), allowed...) {
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
