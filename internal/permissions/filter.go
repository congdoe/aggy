package permissions

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/safefs"
)

// Class says what the filter makes of an allow rule. The filter is an
// allow-list at heart: only a known read-only form applies everywhere, so a
// program aggy has never heard of fails closed and waits for trust.
type Class int

const (
	// Keep is a known read-only form: it applies wherever its source does.
	Keep Class = iota
	// RepoCode may run a repo's own code, or aggy does not know it, so it
	// applies only in a trusted repo.
	RepoCode
	// Drop is never passed on.
	Drop
)

// toolName is what may stand before a rule's parenthesis. Nothing starting
// with a dash can pass, so a rule can never be read as a flag of a CLI.
var toolName = regexp.MustCompile(`^[A-Za-z*][A-Za-z0-9_*-]*$`)

// ParseRule splits a rule into its tool and specifier. Claude writes one as
// Tool or Tool(specifier) (permissions.md, read 2026-11-07).
func ParseRule(rule string) (tool, spec string, hasSpec bool, err error) {
	if rule == "" || strings.TrimSpace(rule) != rule || strings.ContainsAny(rule, "\n\r\x00") {
		return "", "", false, errors.New("a rule is one line with no space around it")
	}
	bad := errors.New("a rule is Tool or Tool(specifier)")
	open := strings.IndexByte(rule, '(')
	if open < 0 {
		if !toolName.MatchString(rule) {
			return "", "", false, bad
		}
		return rule, "", false, nil
	}
	if !toolName.MatchString(rule[:open]) || !strings.HasSuffix(rule, ")") {
		return "", "", false, bad
	}
	return rule[:open], rule[open+1 : len(rule)-1], true, nil
}

// BashCommand returns the command a Bash(...) rule names, with the older
// trailing :* written as " *", which Claude documents as the same thing
// (permissions.md, "Wildcard patterns"), and whether it holds a wildcard.
// ok is false for any other rule, and for a bare Bash.
func BashCommand(rule string) (cmd string, wild, ok bool) {
	tool, spec, has, err := ParseRule(rule)
	if err != nil || tool != "Bash" || !has {
		return "", false, false
	}
	return shellCommand(spec)
}

func shellCommand(spec string) (cmd string, wild, ok bool) {
	if c, cut := strings.CutSuffix(spec, ":*"); cut {
		spec = c + " *"
	}
	return spec, strings.Contains(spec, "*"), true
}

// PrefixForm is a Bash rule as a CLI that matches exact rules as prefixes reads
// it: Bash(sudo sh) also allows sudo sh -c x there. Grok does
// (matches_command_prefix, policy.rs:648 at grok-build 2bdd1d6a). Any other rule
// comes back as it is.
func PrefixForm(rule string) string {
	cmd, wild, ok := BashCommand(rule)
	if !ok || wild {
		return rule
	}
	return "Bash(" + cmd + " *)"
}

// Claude drops these on entering auto mode: "Blanket Bash(*) or
// PowerShell(*)", "Wildcarded interpreters like Bash(python*)",
// "Package-manager run commands", "Agent allow rules", and "Monitor allow
// rules, because Claude Code runs Monitor commands through the shell"
// (permission-modes.md, "How the classifier evaluates actions", read
// 2026-11-07). The page names no full list, so the lists below are aggy's
// reading of those words, and they are not what keeps an unknown program out:
// that is the read-only list, as anything not on it waits for trust.

// wholeTools are tools whose rules aggy never passes, with any specifier. Task
// is another name for Agent (Claude's docs).
var wholeTools = []string{"agent", "task", "monitor"}

// interpreters and shells run whatever text they are given, or a script.
var interpreters = []string{
	"python", "py", "ipython", "bpython", "pypy", "jshell", "ghci", "node", "nodejs", "deno", "bun", "ts-node", "tsx", "ruby", "irb", "perl", "php", "lua", "luajit",
	"julia", "rscript", "r", "elixir", "erl", "clojure", "scala", "kotlin", "groovy", "guile", "racket", "tclsh", "expect",
	"bash", "sh", "zsh", "fish", "dash", "ash", "ksh", "csh", "tcsh", "pwsh", "powershell", "cmd", "busybox", "toybox",
	"osascript", "awk", "gawk", "mawk", "sed", "gdb", "vim", "vi", "nvim", "emacs", "ex", "ed",
	"iex", "invoke-expression", "invoke-command", "start-process",
}

// runners run the command after them, so a wildcard after one is as open as a
// blanket rule.
var runners = []string{
	"env", "sudo", "doas", "su", "xargs", "eval", "exec", "command", "builtin", "nohup", "time", "timeout", "nice", "ionice",
	"stdbuf", "watch", "parallel", "ssh", "script", "setsid", "flock", "find", "caffeinate", "chroot", "tmux", "screen",
	"nix-shell", "unshare", "strace", "ltrace", "dtruss", "fd", "noglob", "nocorrect",
}

// runForms are commands for running something by name, in package managers and
// tools like them. A wildcard form is dropped; an exact one names one thing a
// repo defines, so it waits for trust.
var runForms = []string{
	"npm run", "npm run-script", "npm exec", "npm x", "npx",
	"pnpm run", "pnpm exec", "pnpm dlx", "pnpx",
	"yarn run", "yarn exec", "yarn dlx", "yarn node",
	"bun run", "bun x", "bunx",
	"deno run", "deno task", "deno eval",
	"uv run", "uvx", "uv tool run", "pipx run", "poetry run", "pdm run", "pipenv run", "hatch run", "conda run",
	"cargo run", "go run", "dotnet run", "composer run", "composer exec", "bundle exec", "gem exec", "swift run",
	"docker run", "docker exec", "podman run", "podman exec", "nix run", "nix develop", "nix shell",
	"devbox run", "direnv exec", "mise exec", "mise x", "mise run", "asdf exec",
}

// runVerbs are the words that make a package manager run something, wherever
// flags put them: npm -w x exec, yarn workspace x run.
var runVerbs = []string{"run", "run-script", "exec", "x", "dlx", "eval", "node", "task", "tool", "workspace", "shell", "develop"}

var runPrograms = []string{
	"npm", "pnpm", "yarn", "bun", "deno", "uv", "pipx", "poetry", "pdm", "pipenv", "hatch", "conda", "cargo", "go", "dotnet",
	"composer", "bundle", "gem", "swift", "docker", "podman", "nix", "devbox", "direnv", "mise", "asdf",
}

// readTools read, and a rule for one names what. A bare one waits for trust.
var readTools = []string{"Read", "Grep", "Glob", "WebFetch", "WebSearch"}

// readOnlyAny are read-only with any arguments, so a wildcard is fine.
var readOnlyAny = []string{"ls", "pwd", "cat", "grep", "head", "tail", "wc", "stat", "du", "df", "whoami", "uname", "which"}

// readOnlyGit are the only git commands that apply everywhere, whole and exact:
// the forms OpenCode's starter pins. Any other can run a repo's external diff or
// filters, or write a file with --output.
var readOnlyGit = []string{
	"git status", "git status --short",
	"git diff --no-ext-diff --no-textconv", "git diff --no-ext-diff --no-textconv --stat",
	"git log --oneline -20", "git branch --show-current",
}

// readOnlyPlain are read-only with no flag at all: rg --pre and --hostname-bin
// run a program (rg --help, 15.2.0), and -z starts decompressors; file -C writes
// and -f reads a list. date takes only +format, as date 0101 or -s sets the clock.
var readOnlyPlain = []string{"rg", "file", "date"}

// riskyFlags make a read-only form write a file or run something.
var riskyFlags = []string{"--output", "--pre", "--ext-diff", "--textconv", "--exec", "--open-files-in-pager"}

// neverListed read any path, on top of what interpreters and runners do. OpenCode
// lists none of them, even exact (docs/design.md, section 15, "Whitelist").
var neverListed = []string{"rg", "cat", "head", "tail", "grep", "less", "more", "xxd", "od", "strings", "nl", "dd", "tar", "cp", "mv"}

// versionSuffix is a program's version: python3.12, node22, perl5.34, python3.13t.
var versionSuffix = regexp.MustCompile(`^([a-z][a-z_+-]*?)-?[0-9][0-9.]*t?$`)

// NormalProgram is the program a word names, however it is spelled: \python,
// "python", /usr/bin/python3.12, PYTHON, and python.exe are all python.
func NormalProgram(word string) string {
	word = strings.NewReplacer(`\`, "", `"`, "", "'", "").Replace(word)
	if i := strings.LastIndexByte(word, '/'); i >= 0 {
		word = word[i+1:]
	}
	word = strings.TrimSuffix(strings.ToLower(word), ".exe")
	if m := versionSuffix.FindStringSubmatch(word); m != nil {
		return m[1]
	}
	return word
}

// commandWords are a command's words past any variable it sets first, which a
// shell runs the same way (FOO=1 python x runs python).
func commandWords(cmd string) []string {
	words := strings.Fields(cmd)
	for len(words) > 0 && strings.Contains(words[0], "=") && !strings.ContainsAny(words[0], "*?[") {
		words = words[1:]
	}
	return words
}

// NeverListed reports a command OpenCode's whitelist never takes, exact or not:
// it runs code or reads any path, by the same normalizing as the filter.
func NeverListed(cmd string) bool {
	words := commandWords(cmd)
	if len(words) == 0 {
		return true
	}
	p := NormalProgram(words[0])
	return slices.Contains(interpreters, p) || slices.Contains(runners, p) || slices.Contains(neverListed, p)
}

// CheckAllow classifies an allow rule, and says why when it is not Keep.
func CheckAllow(rule string) (Class, string) {
	tool, spec, has, err := ParseRule(rule)
	if err != nil {
		return Drop, err.Error()
	}
	low := strings.ToLower(tool)
	if has && (low == "bash" || low == "powershell" || slices.Contains(pathTools, low)) && reachesSecret(spec) {
		return Drop, "it reaches your home or a folder of keys"
	}
	switch {
	case slices.Contains(wholeTools, low):
		return Drop, tool + " rules can run anything, and Claude drops them in auto mode"
	case low == "bash":
		// Only Claude's own spelling can be read-only; another can only be held.
		return checkShell(spec, has, tool == "Bash")
	case low == "powershell":
		return checkShell(spec, has, false)
	case strings.Contains(tool, "*") && (!strings.HasPrefix(low, "mcp__") || strings.Trim(low[5:], "*_") == ""):
		return Drop, "a wildcard in a tool's name can match any tool"
	case slices.Contains(readTools, tool) && has && narrowRead(tool, spec):
		return Keep, ""
	}
	return RepoCode, "it is not a known read-only form, so it waits for trust"
}

// checkShell classifies a shell command. It looks at the whole rule as one
// command: TODO(T6955): judge each part of a compound rule (cd x && npm run *).
// For now such a rule is never read-only, so it waits for trust.
func checkShell(spec string, has, readOnlyAllowed bool) (Class, string) {
	if !has {
		return Drop, "blanket shell access"
	}
	cmd, wild, _ := shellCommand(spec)
	if strings.Trim(cmd, "*? ") == "" {
		return Drop, "blanket shell access"
	}
	words := commandWords(cmd)
	if len(words) == 0 {
		return Drop, "a rule that only sets a variable can match any command"
	}
	if strings.ContainsAny(words[0], "*?[") {
		// Bash(python*), Bash(* --help), and Bash(?? *) match programs nobody named.
		return Drop, "a wildcard in the program's name can match any program"
	}
	program := NormalProgram(words[0])
	if form, ok := runForm(program, words[1:], wild); ok {
		if wild {
			return Drop, "a wildcard on a run command (" + form + ") runs whatever it is given"
		}
		return RepoCode, form + " runs what the repo defines"
	}
	switch {
	case program == "git" && gitRunsAnything(words[1:], wild):
		// git -c core.sshCommand=x, git -c alias.x=!id, and git * include any command.
		return Drop, "git with a wildcard or a config option first can run any command"
	case slices.Contains(interpreters, program):
		if wild {
			return Drop, "a wildcarded interpreter runs any code"
		}
		return RepoCode, program + " runs code"
	case slices.Contains(runners, program):
		if wild {
			return Drop, fmt.Sprintf("%s runs a command it is given, so a wildcard here runs any command", program)
		}
		if slices.ContainsFunc(words[1:], func(w string) bool { return slices.Contains(interpreters, NormalProgram(w)) }) {
			return Drop, program + " in front of an interpreter runs any code"
		}
		return RepoCode, program + " runs a command it is given"
	}
	if readOnlyAllowed && isReadOnly(cmd, wild) {
		return Keep, ""
	}
	if strings.Contains(words[0], "/") {
		return RepoCode, "it runs a program by its path, so it waits for trust"
	}
	return RepoCode, "it is not a known read-only form, so it waits for trust"
}

func gitRunsAnything(args []string, wild bool) bool {
	if len(args) == 0 {
		return wild
	}
	first := args[0]
	if slices.Contains([]string{"-c", "--config-env", "--exec-path"}, first) || strings.HasPrefix(first, "--config-env=") || strings.HasPrefix(first, "--exec-path=") {
		return true
	}
	return strings.HasPrefix(first, "*") || (wild && strings.HasPrefix(first, "-"))
}

// runForm finds the run form a command is, or one a wildcard rule could reach:
// Bash(npm *) includes npm run, and so do npm ru* and npm --prefix . *.
func runForm(program string, args []string, wild bool) (string, bool) {
	words := append([]string{program}, args...)
	fixed := words
	if i := slices.IndexFunc(words, func(w string) bool { return strings.ContainsAny(w, "*?[") }); i >= 0 {
		fixed = words[:i]
	}
	for _, form := range runForms {
		fw := strings.Fields(form)
		if len(words) >= len(fw) && slices.Equal(words[:len(fw)], fw) {
			return form, true
		}
		if wild && len(fixed) < len(fw) && slices.Equal(fixed, fw[:len(fixed)]) {
			return form, true
		}
	}
	if !slices.Contains(runPrograms, program) {
		return "", false
	}
	for _, w := range args {
		if slices.Contains(runVerbs, w) {
			return program + " " + w, true
		}
	}
	if !wild {
		return "", false
	}
	if len(args) > 0 && strings.HasPrefix(args[0], "-") {
		return program + " with an option first", true
	}
	// A wildcard inside the subcommand's own word: npm ru*, npm t*.
	if len(fixed) == 1 && len(args) > 0 {
		head, _, _ := strings.Cut(args[0], "*")
		if head != "" && slices.ContainsFunc(runVerbs, func(v string) bool { return strings.HasPrefix(v, head) }) {
			return program + " " + args[0], true
		}
	}
	return "", false
}

// isReadOnly reports one of the known read-only forms, written exactly: no path,
// no capitals, no leading variable, no operator, and no flag that writes or runs.
func isReadOnly(cmd string, wild bool) bool {
	if strings.ContainsAny(cmd, ";|&<>`$()\\\"'\n") {
		return false
	}
	words := strings.Fields(cmd)
	for _, w := range words {
		if slices.ContainsFunc(riskyFlags, func(f string) bool { return strings.HasPrefix(w, f) }) {
			return false
		}
	}
	if len(words) == 0 {
		return false
	}
	switch first := words[0]; {
	case first == "git":
		return !wild && slices.Contains(readOnlyGit, cmd)
	case first == "date":
		return !wild && !slices.ContainsFunc(words[1:], func(w string) bool { return !strings.HasPrefix(w, "+") })
	case slices.Contains(readOnlyPlain, first):
		return !wild && !slices.ContainsFunc(words[1:], func(w string) bool { return strings.HasPrefix(w, "-") })
	}
	return slices.Contains(readOnlyAny, words[0])
}

// StaysReadOnlyAsPrefix says an exact rule is still read-only on a CLI that takes
// it as a prefix (see PrefixForm). git status has no flag that writes or runs.
func StaysReadOnlyAsPrefix(rule string) bool {
	if cmd, _, ok := BashCommand(rule); ok && cmd == "git status" {
		return true
	}
	class, _ := CheckAllow(PrefixForm(rule))
	return class == Keep
}

// pathTools take a path as their specifier.
var pathTools = []string{"read", "edit", "write", "multiedit", "notebookedit", "grep", "glob"}

// reachesSecret reports a specifier or command with a word that reaches the home
// or a folder safefs guards, by name or by what it resolves to. It reads no file.
func reachesSecret(spec string) bool {
	home, _ := os.UserHomeDir()
	for _, word := range strings.Fields(spec) {
		word = strings.Trim(word, `"'`)
		if i := strings.IndexAny(word, "*?["); i >= 0 {
			word = word[:i]
		}
		for _, prefix := range []string{"~", "$HOME", "${HOME}"} {
			if rest, ok := strings.CutPrefix(word, prefix); ok && home != "" && (rest == "" || rest[0] == '/') {
				word = home + rest
			}
		}
		if slices.ContainsFunc(safefs.SecretHomeDirs(), func(d string) bool { return strings.Contains(word, d) }) {
			return true
		}
		if strings.HasPrefix(word, "~") || strings.HasPrefix(word, "$HOME") || strings.HasPrefix(word, "${HOME}") {
			return true // no home to resolve it by
		}
		if strings.HasPrefix(word, "//") {
			word = word[1:]
		}
		if strings.HasPrefix(word, "/") {
			if holds, err := safefs.HoldsSecrets(word); err == nil && holds {
				return true
			}
		}
	}
	return false
}

// narrowRead says a read tool's specifier names something in particular: a path
// under the repo, starting with a real folder or file, or one site by name.
func narrowRead(tool, spec string) bool {
	switch tool {
	case "WebFetch":
		host, ok := strings.CutPrefix(spec, "domain:")
		return ok && strings.Contains(host, ".") && !strings.ContainsAny(host, "*?[/ ")
	case "WebSearch":
		return strings.Trim(spec, "* ") != "" && !strings.Contains(spec, "*")
	}
	spec = strings.TrimPrefix(spec, "./")
	first, _, _ := strings.Cut(spec, "/")
	return first != "" && first != ".." && !strings.Contains(spec, "/../") &&
		!strings.ContainsAny(first, "*?[~$") && !strings.HasPrefix(spec, "/")
}

// Modes aggy passes on. bypassPermissions is left out: aggy never adds a
// loosening setting on its own (docs/design.md, "What stays out"). Grok lists
// the same names (user-guide/22-permissions-and-safety.md at grok-build
// 2bdd1d6a); manual is Claude's other name for default (permission-modes.md).
var modes = []string{"default", "acceptEdits", "plan", "auto", "dontAsk"}

// CheckMode returns the mode as aggy spells it, or why it will not pass it on.
func CheckMode(mode string) (string, error) {
	if mode == "manual" {
		return "default", nil
	}
	if slices.Contains(modes, mode) {
		return mode, nil
	}
	return "", fmt.Errorf("%q is not a mode aggy passes on: use %s", mode, strings.Join(modes, ", "))
}
