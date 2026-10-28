package tooldef

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/congdoe/aggy/internal/hook"
)

// Program names, compared after case folding (macOS file systems ignore
// case, so /BIN/SH runs sh) and without a .exe suffix. This is defence
// in depth: a definition's program is arbitrary code whatever its name,
// and the preview on add and the explicit turn-on are the real gate.
var (
	// shellPrefixes match a shell by name and any suffix (zsh-5.9,
	// bash5, pwsh-preview).
	shellPrefixes = []string{"bash", "zsh", "dash", "ksh", "mksh", "tcsh", "csh", "fish", "pwsh", "powershell",
		"rbash", "rksh", "rzsh", "yash", "xonsh", "elvish", "nushell"}
	// shellNames match a shell by name, or name and a non-letter suffix
	// (sh5, sh-x), so shasum and nu-free names such as nudge still pass.
	shellNames = []string{"sh", "ash", "nu", "osh", "cmd", "command.com"}
	// neverPrograms run a string as a command, or as another user.
	neverPrograms = []string{"env", "sudo", "doas", "su", "ssh", "eval", "exec", "builtin", "command", "busybox",
		"toybox", "watch", "parallel", "script", "run0", "pkexec", "start-stop-daemon", "setsid",
		// These run commands named in their arguments or script text
		// (sed's e command, awk's system(), vim -c, lldb -o, gdb -ex,
		// expect's Tcl, tmux and screen sessions, launchctl jobs, and
		// sandbox-exec and xcrun, which run the command they are given),
		// and none is a tool's program.
		"sed", "gsed", "awk", "gawk", "mawk", "nawk", "vi", "vim", "nvim", "view", "ex", "lldb", "gdb",
		"expect", "tmux", "screen", "launchctl", "sandbox-exec", "xcrun"}
	// interpreters run code given with -c, -e, -E, --eval, or eval.
	interpreters = []string{"python", "perl", "ruby", "node", "nodejs", "php", "lua", "luajit", "osascript",
		"tclsh", "wish", "rscript", "julia", "deno", "bun", "jshell", "irb", "groovy"}
	// codeFlags give an interpreter its code inline.
	codeFlags = []string{"-c", "-e", "-E", "--eval", "--command", "--exec", "-r", "eval", "--print", "-p"}
	// loadFlags make an interpreter load a file or module first; their
	// value may not be the repo's code.
	loadFlags = []string{"--require", "--import", "--loader", "--experimental-loader", "-I", "-M", "--include"}
)

// Package runners and managers. A runner fetches a package and runs it,
// so its package must be pinned and it may never install without asking
// (-y) or from another index; a manager's install or run subcommands
// run the repo's or a registry's code.
var (
	// fetchers run a package by themselves.
	fetchers = []string{"npx", "pnpx", "bunx", "uvx"}
	// managers run a package through a subcommand, or install one.
	managers = []string{"npm", "pnpm", "yarn", "pip", "pip3", "pipx", "uv", "cargo", "go"}
	// installSubs install packages or run the repo's scripts.
	installSubs = []string{"install", "i", "add", "run", "start", "test", "ci", "get", "update", "up", "upgrade", "run-script"}
	// refusedFetchFlags install without asking, from another index, a
	// local file, or a shell string.
	refusedFetchFlags = []string{"-y", "--yes", "--registry", "--index-url", "--index", "--extra-index-url",
		"--default-index", "--find-links", "-i", "--pip-args", "--with-requirements", "--with-editable", "-c", "--call"}
	npmValues = []string{"-p", "--package", "--cache", "--userconfig", "--prefix", "-w", "--workspace", "-C", "--dir"}
	uvValues  = []string{"-p", "--python", "--from", "--with", "--cache-dir", "--directory", "--project",
		"--config-file", "--constraints", "--overrides"}
)

// runner is how one package runner takes its flags: vals take a value,
// and pkg are those whose value names a package.
type runner struct{ vals, pkg []string }

var runners = map[string]runner{
	"npx": {npmValues, []string{"-p", "--package"}}, "pnpx": {npmValues, []string{"-p", "--package"}},
	"bunx": {npmValues, []string{"-p", "--package"}},
	"uvx":  {uvValues, []string{"--from", "--with"}}, "pipx": {[]string{"--spec", "--python"}, []string{"--spec"}},
}

var (
	semverRE  = regexp.MustCompile(`^\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$`)
	pkgNameRE = regexp.MustCompile(`^(@[a-z0-9][a-z0-9._-]*/)?[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// normProgram is the base name of p, case folded as a case-insensitive
// file system may (full Unicode folding after compatibility
// normalization, so ſh and SH are both sh), without .exe.
func normProgram(p string) string {
	return strings.TrimSuffix(cases.Fold().String(norm.NFKC.String(path.Base(p))), ".exe")
}

func isShell(n string) bool {
	for _, s := range shellPrefixes {
		if strings.HasPrefix(n, s) {
			return true
		}
	}
	for _, s := range shellNames {
		if rest, ok := strings.CutPrefix(n, s); ok && (rest == "" || !unicode.IsLetter(rune(rest[0]))) {
			return true
		}
	}
	return false
}

func hasPrefixIn(n string, xs []string) bool {
	return slices.ContainsFunc(xs, func(x string) bool { return strings.HasPrefix(n, x) })
}

// runsString says why the word w names a program that runs a string, or "".
func runsString(w string) string {
	n := normProgram(w)
	switch {
	case isShell(n):
		return fmt.Sprintf("%q is a shell, which would run a shell string", w)
	case slices.Contains(neverPrograms, n):
		return fmt.Sprintf("%q runs a string as a command, or as another user", w)
	}
	return ""
}

// checkPrograms refuses a shell, env, or other command runner at the
// program, or anywhere after a wrapper (nice, timeout, xargs, ...); an
// interpreter given inline code or the repo's own script; a package
// runner that would fetch unpinned code; and {repo} wherever it could be
// the program a wrapper runs.
func checkPrograms(a []string) string {
	wrapped := false
	for i := 0; i < len(a); i++ {
		n := normProgram(a[i])
		if why := runsString(a[i]); why != "" {
			return why
		}
		if wrapped && strings.Contains(a[i], Placeholder) {
			// Shortcut: any word after a wrapper may be the program it
			// runs, so {repo} is refused in all of them.
			return fmt.Sprintf("%q after a wrapper could be the program it runs: the repo's own code", a[i])
		}
		switch {
		case n == "find":
			for _, f := range a[i+1:] {
				if slices.Contains([]string{"-exec", "-execdir", "-ok", "-okdir"}, f) {
					return fmt.Sprintf("%q runs a command with %q", a[i], f)
				}
			}
		case n == "git":
			// -c core.sshCommand=..., alias.x=!..., core.pager and the
			// like run a command from a config value.
			for _, f := range a[i+1:] {
				if strings.HasPrefix(f, "-c") || strings.HasPrefix(f, "--config-env") {
					return fmt.Sprintf("%q is given a config value with %q, which can run a command", a[i], f)
				}
			}
		case n == "flock":
			for _, f := range a[i+1:] {
				if f == "-c" || f == "--command" || strings.HasPrefix(f, "--command=") {
					return fmt.Sprintf("%q is given a shell string with %q", a[i], f)
				}
			}
			wrapped = true
			continue
		case slices.Contains(fetchers, n):
			return checkFetch(a[i], runners[n], a[i+1:])
		case slices.Contains(managers, n):
			return checkManager(a[i], n, a[i+1:])
		case hook.IsWrapper(n):
			wrapped = true
			continue // what the wrapper runs is checked too
		case hasPrefixIn(n, interpreters):
			return checkInterpreter(a[i], n, a[i+1:])
		}
		if i > 0 {
			continue // an argument after a wrapper: keep looking for a runner
		}
		return ""
	}
	return ""
}

// checkInterpreter refuses inline code, and the repo's own code as the
// script or as a module loaded first (node --require, perl -I, ...).
func checkInterpreter(prog, n string, rest []string) string {
	python := strings.HasPrefix(n, "python")
	// Short flags whose value follows, in the same word or the next.
	valueChars := "WXMmIrCxl"
	if python {
		valueChars = "WXmQ" // python -I takes no value
	}
	for k := 0; k < len(rest); k++ {
		f := rest[k]
		if codeFlag(f, valueChars) {
			return fmt.Sprintf("%q is given inline code with %q", prog, f)
		}
		name, val, hasVal := strings.Cut(f, "=")
		switch {
		case !python && slices.Contains(loadFlags, name), python && f == "-m":
			if !hasVal && k+1 < len(rest) {
				k++
				val = rest[k]
			}
			if strings.Contains(val, Placeholder) {
				return fmt.Sprintf("%q would load the repo's own code with %q", prog, name)
			}
			if python && f == "-m" {
				return "" // the rest are the module's arguments
			}
		case f == "--":
			if k+1 < len(rest) && strings.Contains(rest[k+1], Placeholder) {
				return fmt.Sprintf("%q would run the repo's own script", prog)
			}
			return ""
		case strings.HasPrefix(f, "-"):
			if len(f) == 2 && strings.ContainsRune(valueChars, rune(f[1])) {
				k++ // its value
			}
		default:
			if strings.Contains(f, Placeholder) {
				return fmt.Sprintf("%q would run the repo's own script", prog)
			}
			return "" // the script; the rest are its arguments
		}
	}
	return ""
}

// codeFlag reports an inline-code flag: one of codeFlags, its --x=value
// form, or a short cluster that holds c, e, or E as a flag of its own
// (python -Ic, perl -we), not inside a value (python -Wignore, perl
// -MO=Deparse).
func codeFlag(f, valueChars string) bool {
	name, _, _ := strings.Cut(f, "=")
	if slices.Contains(codeFlags, name) {
		return true
	}
	if len(f) < 3 || f[0] != '-' || f[1] == '-' {
		return false
	}
	for _, c := range f[1:] {
		switch {
		case strings.ContainsRune("ceE", c):
			return true
		case strings.ContainsRune(valueChars, c):
			return false // the rest of the word is this flag's value
		}
	}
	return false
}

// checkManager handles npm, pnpm, yarn, pip, uv, cargo, and go: never
// with -y or another index, never install or run subcommands, and their
// package runners (npm exec, pnpm dlx, yarn dlx, uv tool run, pipx run)
// checked as fetchers.
func checkManager(prog, n string, rest []string) string {
	sub := -1
	for k := 0; k < len(rest); k++ {
		f := rest[k]
		name, _, hasVal := strings.Cut(f, "=")
		if slices.Contains(refusedFetchFlags, name) {
			return fmt.Sprintf("%q with %q installs or runs a package without asking, or from another index", prog, f)
		}
		if strings.HasPrefix(f, "-") {
			if !hasVal && slices.Contains(npmValues, name) {
				k++
			}
			continue
		}
		sub = k
		break
	}
	if sub < 0 {
		if n == "yarn" {
			return fmt.Sprintf("%q alone installs the repo's packages", prog)
		}
		return ""
	}
	s, after := rest[sub], rest[sub+1:]
	switch {
	case (n == "npm" && (s == "exec" || s == "x")) || (n == "pnpm" && (s == "dlx" || s == "exec")) || (n == "yarn" && s == "dlx"):
		return checkFetch(prog+" "+s, runners["npx"], after)
	case n == "uv" && s == "tool" && len(after) > 0 && after[0] == "run":
		return checkFetch(prog+" tool run", runners["uvx"], after[1:])
	case n == "pipx" && s == "run":
		return checkFetch(prog+" run", runners["pipx"], after)
	case slices.Contains(installSubs, s):
		return fmt.Sprintf("%q %q installs packages or runs the repo's scripts", prog, s)
	}
	return ""
}

// checkFetch refuses a package runner that would fetch new code on a
// launch, which breaks "never install unasked": -y or --yes, another
// index or a local file, a shell string, and a package not pinned to an
// exact version. The command it then runs is checked too.
func checkFetch(prog string, r runner, rest []string) string {
	named := false // a --package or --from names the package
	for k := 0; k < len(rest); k++ {
		f := rest[k]
		name, val, hasVal := strings.Cut(f, "=")
		switch {
		case slices.Contains(refusedFetchFlags, name):
			return fmt.Sprintf("%q with %q installs or runs a package without asking, or from another index", prog, f)
		case slices.Contains(r.vals, name):
			if !hasVal {
				if k+1 >= len(rest) {
					return fmt.Sprintf("%q: %q needs a value", prog, f)
				}
				k++
				val = rest[k]
			}
			if slices.Contains(r.pkg, name) {
				if !pinnedSpec(val) {
					return fmt.Sprintf("%q runs %q, which is not a package pinned to a version (want name@1.2.3)", prog, val)
				}
				named = named || name != "--with" // --with adds a dependency only
			}
		case f == "--":
		case strings.HasPrefix(f, "-"):
		default:
			if !named && !pinnedSpec(f) {
				return fmt.Sprintf("%q runs %q, which is not a package pinned to a version (want name@1.2.3)", prog, f)
			}
			// f is the command the runner starts; the rest its arguments.
			return runsString(f)
		}
	}
	return ""
}

// pinnedSpec reports whether a package spec is a plain package name and
// an exact version: name@1.2.3, @scope/name@1.2.3-beta.1, or
// name==1.2.3. Never a tag (latest), a range, a URL, git+..., or a path.
func pinnedSpec(spec string) bool {
	name, v, ok := strings.Cut(spec, "==")
	if !ok {
		i := strings.LastIndexByte(spec, '@')
		if i <= 0 {
			return false
		}
		name, v = spec[:i], spec[i+1:]
	}
	return pkgNameRE.MatchString(name) && semverRE.MatchString(v)
}
