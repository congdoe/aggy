package permissions

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Class says what the filter makes of an allow rule.
type Class int

const (
	// Keep is an allow that applies wherever its source does.
	Keep Class = iota
	// RepoCode runs the repo's own code, so it applies only in a trusted repo.
	RepoCode
	// Drop is never passed on.
	Drop
)

// ParseRule splits a rule into its tool and specifier. Claude writes one as
// Tool or Tool(specifier) (permissions.md, read 2026-11-07).
func ParseRule(rule string) (tool, spec string, hasSpec bool, err error) {
	if rule == "" || strings.TrimSpace(rule) != rule || strings.ContainsAny(rule, "\n\r\x00") {
		return "", "", false, errors.New("a rule is one line with no space around it")
	}
	open := strings.IndexByte(rule, '(')
	if open < 0 {
		if strings.ContainsAny(rule, ") ") {
			return "", "", false, errors.New("a rule is Tool or Tool(specifier)")
		}
		return rule, "", false, nil
	}
	if open == 0 || !strings.HasSuffix(rule, ")") {
		return "", "", false, errors.New("a rule is Tool or Tool(specifier)")
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
	if c, cut := strings.CutSuffix(spec, ":*"); cut {
		spec = c + " *"
	}
	return spec, strings.Contains(spec, "*"), true
}

// Claude drops these on entering auto mode: "Blanket Bash(*) or
// PowerShell(*)", "Wildcarded interpreters like Bash(python*)",
// "Package-manager run commands", "Agent allow rules", and "Monitor allow
// rules, because Claude Code runs Monitor commands through the shell"
// (permission-modes.md, "How the classifier evaluates actions", read
// 2026-11-07). It names no full list, so the lists below are aggy's reading
// of those words, not a copy of Claude's. A word missing from them only means
// the rule reaches a CLI as the person wrote it; deny rules still win.

// wholeTools are tools whose rules aggy never passes, with any specifier.
var wholeTools = []string{"Agent", "Monitor"}

// interpreters run whatever text they are given. wrappers run the command
// after them, so a wildcard after one is as open as a blanket rule.
var interpreters = []string{
	"python", "python2", "python3", "pypy", "pypy3", "node", "nodejs", "deno", "bun", "ts-node", "tsx",
	"ruby", "irb", "perl", "php", "lua", "bash", "sh", "zsh", "fish", "dash", "ksh", "csh", "tcsh",
	"pwsh", "powershell", "osascript", "awk", "gawk", "mawk",
}

var wrappers = []string{
	"env", "sudo", "doas", "su", "xargs", "eval", "exec", "command", "builtin", "nohup", "time", "timeout",
	"nice", "ionice", "stdbuf", "watch", "parallel", "ssh", "script",
}

// runForms are package managers' commands for running something by name.
var runForms = []string{
	"npm run", "npm run-script", "npm exec", "npm x", "npx",
	"pnpm run", "pnpm exec", "pnpm dlx", "pnpx",
	"yarn run", "yarn exec", "yarn dlx", "yarn node",
	"bun run", "bun x", "bunx",
	"deno run", "deno task", "deno eval",
	"uv run", "uvx", "uv tool run", "pipx run", "poetry run", "pdm run", "pipenv run", "hatch run", "conda run",
	"cargo run", "go run", "dotnet run", "composer run", "composer exec", "bundle exec", "gem exec", "swift run",
}

// repoRunners run what the repo defines: a build file, a test suite, a script.
var repoRunners = []string{
	"make", "gmake", "just", "task", "rake", "ninja", "cmake", "gradle", "gradlew", "mvn", "mvnw", "ant", "bazel", "tox", "nox",
	"pytest", "jest", "vitest", "mocha", "rspec", "phpunit", "tsc", "eslint", "webpack", "vite", "turbo", "nx",
	"npm", "pnpm", "yarn", "cargo", "go", "dotnet", "composer", "bundle", "gem", "pip", "pip3", "swift",
	"source", ".",
}

// CheckAllow classifies an allow rule, and says why when it is not Keep.
func CheckAllow(rule string) (Class, string) {
	tool, spec, has, err := ParseRule(rule)
	if err != nil {
		return Drop, err.Error()
	}
	switch {
	case slices.Contains(wholeTools, tool):
		return Drop, tool + " rules can run anything, and Claude drops them in auto mode"
	case tool == "PowerShell":
		if !has || strings.Trim(spec, "* ") == "" {
			return Drop, "blanket shell access"
		}
		return Keep, ""
	case tool != "Bash":
		return Keep, ""
	case !has:
		return Drop, "blanket shell access"
	}
	cmd, wild, _ := BashCommand(rule)
	if strings.Trim(cmd, "* ") == "" {
		return Drop, "blanket shell access"
	}
	first, _, _ := strings.Cut(strings.TrimSpace(cmd), " ")
	if strings.Contains(first, "*") {
		// Bash(python*) and Bash(* --help) both match programs nobody named.
		return Drop, "a wildcard in the program's name can match any program"
	}
	if form, ok := runFormOf(cmd, wild); ok {
		return Drop, "a package manager's run command (" + form + ") runs whatever it is given"
	}
	switch {
	case wild && slices.Contains(interpreters, first):
		return Drop, "a wildcarded interpreter runs any code"
	case wild && slices.Contains(wrappers, first):
		return Drop, fmt.Sprintf("%s runs the command after it, so a wildcard here runs any command", first)
	case slices.Contains(interpreters, first):
		return RepoCode, first + " runs code"
	case strings.Contains(first, "/"):
		return RepoCode, "it runs a program by its path"
	case slices.Contains(repoRunners, first):
		return RepoCode, first + " runs what the repo defines"
	}
	return Keep, ""
}

// runFormOf finds the run form cmd is, or, for a wildcard rule, one its fixed
// words could still reach: Bash(npm *) includes npm run.
func runFormOf(cmd string, wild bool) (string, bool) {
	words := strings.Fields(cmd)
	for _, form := range runForms {
		fw := strings.Fields(form)
		if len(words) >= len(fw) && slices.Equal(words[:len(fw)], fw) {
			return form, true
		}
		if !wild {
			continue
		}
		fixed := words
		if i := slices.IndexFunc(words, func(w string) bool { return strings.Contains(w, "*") }); i >= 0 {
			fixed = words[:i]
		}
		if len(fixed) < len(fw) && slices.Equal(fixed, fw[:len(fixed)]) {
			return form, true
		}
	}
	return "", false
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
