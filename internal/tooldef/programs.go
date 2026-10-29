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
		"tclsh", "wish", "rscript", "julia", "deno", "bun", "jshell", "irb", "groovy",
		// java runs the jar or class it is given: checked as a script,
		// so -jar's file is absolute and a bare class name is refused
		// (it would load from the working directory; W71-2).
		"java"}
	// codeFlags give an interpreter its code inline.
	codeFlags = []string{"-c", "-e", "-E", "--eval", "--command", "--exec", "-r", "eval", "--print", "-p"}
	// nodeLoadFlags make node, deno, or bun load a module first. A bare
	// name resolves from the working directory's node_modules, which may
	// be the repo's, so the value must be an absolute path.
	nodeLoadFlags = []string{"--require", "-r", "--import", "--loader", "--experimental-loader", "--preload"}
	nodeFamily    = []string{"node", "nodejs", "deno", "bun"}
	// nodeConfigFlags make deno or bun read a config or an import map;
	// a relative one is read from the working directory (W71-2).
	nodeConfigFlags = []string{"--config", "--import-map"}
)

// Package runners and managers. A runner fetches a package and runs it,
// so its package must be pinned, and it takes only the flags listed
// here (S63-12): any other may take a value that swallows the pin,
// install without asking (-y), or fetch from another index. A manager
// is allowed only as a package runner (S63-13): its other subcommands
// install packages or run the repo's code (scripts, build.rs, go
// generate).
var (
	// fetchers run a package by themselves.
	fetchers = []string{"npx", "pnpx", "bunx", "uvx"}
	// managers run a package through a subcommand, or install one.
	managers = []string{"npm", "pnpm", "yarn", "pip", "pip3", "pipx", "uv", "cargo", "go"}
	// managerFlags are the flags a manager may take with no subcommand
	// (npm --version).
	managerFlags = []string{"--version", "-v", "-V", "--help", "-h"}
	npmBools     = []string{"-q", "--quiet", "--silent", "--offline", "--prefer-offline", "--no-update-notifier"}
	uvBools      = []string{"-q", "--quiet", "--offline", "--no-cache", "--isolated", "--no-progress"}
)

// runner is how one package runner takes its flags: bools take no
// value, vals take one, and pkg are those whose value names a package.
type runner struct{ bools, vals, pkg []string }

var (
	npmRunner = runner{npmBools, []string{"-p", "--package"}, []string{"-p", "--package"}}
	uvRunner  = runner{uvBools, []string{"--from", "--with", "-p", "--python"}, []string{"--from", "--with"}}
	runners   = map[string]runner{
		"npx": npmRunner, "pnpx": npmRunner, "bunx": npmRunner, "uvx": uvRunner,
		"pipx": {[]string{"-q", "--quiet"}, []string{"--spec", "--python"}, []string{"--spec"}},
	}
)

// managerRunners are the subcommands that make a manager a package
// runner: npm exec and x, pnpm dlx, yarn dlx, uv tool run, pipx run.
// pnpm exec is not one: it runs the repo's own node_modules/.bin.
var managerRunners = map[string]string{"npm exec": "npx", "npm x": "npx", "pnpm dlx": "npx", "yarn dlx": "npx",
	"uv tool": "uvx", "pipx run": "pipx"}

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
			for k, f := range a[i+1:] {
				if strings.HasPrefix(f, "-c") || strings.HasPrefix(f, "--config-env") {
					return fmt.Sprintf("%q is given a config value with %q, which can run a command", a[i], f)
				}
				// S63-14: --exec-path with its value in the next word.
				if f == "--exec-path" && i+2+k < len(a) && strings.Contains(a[i+2+k], Placeholder) {
					return fmt.Sprintf("%q --exec-path %q would run programs from the repo", a[i], a[i+2+k])
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

// checkInterpreter refuses inline code; the repo's own code as the
// script, or anywhere before it (a module loaded first, perl -I, ruby
// -C); a script that is not an absolute path, which would be the repo's
// when the server runs in it (S63-14; Claude starts MCP servers in the
// session's directory); python -m without -I or -P, which puts that
// directory first on sys.path; and a node, deno, or bun module loaded
// first by a bare name, which resolves from it.
func checkInterpreter(prog, n string, rest []string) string {
	python := strings.HasPrefix(n, "python")
	node := hasPrefixIn(n, nodeFamily)
	// Short flags whose value follows, in the same word or the next.
	valueChars := "WXMmIrCxl"
	if python {
		valueChars = "WXmQ" // python -I takes no value
	}
	isolated := false // python -I or -P was given
	ranSub := false   // deno or bun's run subcommand was skipped
	script := func(f string) string {
		switch {
		case strings.Contains(f, Placeholder):
			return fmt.Sprintf("%q would run the repo's own script", prog)
		case !strings.HasPrefix(f, "/"):
			return fmt.Sprintf("%q runs %q: a script must be an absolute path, since the server may run in the repo, where a relative one is the repo's", prog, f)
		}
		return "" // the rest are its arguments
	}
	for k := 0; k < len(rest); k++ {
		f := rest[k]
		if codeFlag(f, valueChars) {
			return fmt.Sprintf("%q is given inline code with %q", prog, f)
		}
		switch {
		case f == "--":
			if k+1 < len(rest) {
				return script(rest[k+1])
			}
			return ""
		case strings.HasPrefix(f, "-") && f != "-":
			name, val, hasVal := strings.Cut(f, "=")
			load := node && (slices.Contains(nodeLoadFlags, name) || slices.Contains(nodeConfigFlags, name))
			if !hasVal && k+1 < len(rest) && (load || f == "-m" || len(f) == 2 && strings.ContainsRune(valueChars, rune(f[1]))) {
				k++
				val = rest[k]
			}
			if strings.Contains(f, Placeholder) || strings.Contains(val, Placeholder) {
				return fmt.Sprintf("%q would load or run the repo's own code with %q", prog, name)
			}
			if load && !strings.HasPrefix(val, "/") {
				return fmt.Sprintf("%q %s %q: a module loaded first must be an absolute path, not resolved from the working directory", prog, name, val)
			}
			if op, ok := loaderOperand(f, val, python || node); ok && !strings.HasPrefix(op, "/") {
				return fmt.Sprintf("%q %s %q: a library loaded first must be an absolute path, not found from the working directory", prog, f, op)
			}
			if python {
				if f == "-m" {
					if !isolated {
						return fmt.Sprintf("%q -m needs -I or -P: otherwise the working directory, which may be the repo, comes first on sys.path", prog)
					}
					return "" // the rest are the module's arguments
				}
				isolated = isolated || pyIsolates(f)
			}
		case !ranSub && f == "run" && (strings.HasPrefix(n, "deno") || strings.HasPrefix(n, "bun")):
			ranSub = true
		default:
			return script(f)
		}
	}
	return ""
}

// loaderOperand returns what -l (lua, ruby) or -d (perl -d:Module)
// loads, attached or in val, for an interpreter other than python and
// the node family, which take neither as a loader (W71-2).
func loaderOperand(f, val string, skip bool) (string, bool) {
	if skip || len(f) < 2 || f[0] != '-' || f[1] != 'l' && f[1] != 'd' {
		return "", false
	}
	switch {
	case len(f) > 2:
		return strings.TrimPrefix(f[2:], ":"), true
	case f == "-l":
		return val, true
	}
	return "", false // -d alone loads the debugger, from no path
}

// pyIsolates reports a python flag cluster holding -I or -P, before any
// flag that takes a value.
func pyIsolates(f string) bool {
	if len(f) < 2 || f[0] != '-' || f[1] == '-' {
		return false
	}
	for _, c := range f[1:] {
		switch {
		case c == 'I' || c == 'P':
			return true
		case strings.ContainsRune("WXmQ", c):
			return false
		}
	}
	return false
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

// checkManager handles npm, pnpm, yarn, pip, uv, cargo, and go: only as
// a package runner (npm exec, pnpm dlx, yarn dlx, uv tool run, pipx
// run), checked as a fetcher, or with no subcommand and only
// managerFlags (npm --version). Every other subcommand is refused, so a
// new install or run spelling is never let through (S63-13).
func checkManager(prog, n string, rest []string) string {
	sub := 0
	for ; sub < len(rest) && strings.HasPrefix(rest[sub], "-"); sub++ {
		if !slices.Contains(managerFlags, rest[sub]) {
			return fmt.Sprintf("%q takes no flag %q before its subcommand", prog, rest[sub])
		}
	}
	if sub == len(rest) {
		if n == "yarn" && sub == 0 {
			return fmt.Sprintf("%q alone installs the repo's packages", prog)
		}
		return ""
	}
	s, after := rest[sub], rest[sub+1:]
	r, ok := managerRunners[n+" "+s]
	if ok && n == "uv" {
		if len(after) == 0 || after[0] != "run" {
			ok = false
		} else {
			s, after = "tool run", after[1:]
		}
	}
	if !ok {
		return fmt.Sprintf("%q %q: a package manager is allowed only as a package runner (npm exec, pnpm dlx, yarn dlx, uv tool run, pipx run); its other subcommands install packages or run the repo's code", prog, s)
	}
	return checkFetch(prog+" "+s, runners[r], after)
}

// checkFetch refuses a package runner that would fetch new code on a
// launch, which breaks "never install unasked": any flag but r's (so no
// -y, another index, a local file, or a shell string), {repo} before the
// command, and a package not pinned to an exact version. The command it
// then runs is checked too: with the package named by a flag, it is a
// bare command name, never a path (S63-14: npx -p x@1.0.0 -- {repo}/x).
func checkFetch(prog string, r runner, rest []string) string {
	named := false // a --package or --from names the package
	command := func(f string) string {
		if named && (strings.Contains(f, "/") || strings.Contains(f, Placeholder) || f == "." || f == "..") {
			return fmt.Sprintf("%q runs %q: the command must be a name from the package, not a path", prog, f)
		}
		if !named && !pinnedSpec(f) {
			return fmt.Sprintf("%q runs %q, which is not a package pinned to a version (want name@1.2.3)", prog, f)
		}
		// f is the command the runner starts; the rest its arguments.
		return runsString(f)
	}
	for k := 0; k < len(rest); k++ {
		f := rest[k]
		name, val, hasVal := strings.Cut(f, "=")
		if strings.Contains(f, Placeholder) && strings.HasPrefix(f, "-") {
			return fmt.Sprintf("%q with %q would take the repo's code or config", prog, f)
		}
		switch {
		case f == "--":
			if k+1 < len(rest) {
				return command(rest[k+1])
			}
			return ""
		case slices.Contains(r.bools, f):
		case slices.Contains(r.vals, name):
			if !hasVal {
				if k+1 >= len(rest) {
					return fmt.Sprintf("%q: %q needs a value", prog, f)
				}
				k++
				val = rest[k]
			}
			if strings.Contains(val, Placeholder) {
				return fmt.Sprintf("%q %s %q would take the repo's code", prog, name, val)
			}
			// W71-2: an interpreter named by a relative path is found
			// from the working directory; a version or a bare name is not.
			if (name == "--python" || name == "-p") && !slices.Contains(r.pkg, name) &&
				(strings.HasPrefix(val, ".") || strings.Contains(val, "/") && !strings.HasPrefix(val, "/")) {
				return fmt.Sprintf("%q %s %q: an interpreter path must be absolute", prog, name, val)
			}
			if slices.Contains(r.pkg, name) {
				if !pinnedSpec(val) {
					return fmt.Sprintf("%q runs %q, which is not a package pinned to a version (want name@1.2.3)", prog, val)
				}
				named = named || name != "--with" // --with adds a dependency only
			}
		case strings.HasPrefix(f, "-"):
			return fmt.Sprintf("%q with %q: a package runner takes only the flags aggy knows (%s), so no flag can swallow the pin, install without asking, or fetch from elsewhere",
				prog, f, strings.Join(slices.Concat(r.bools, r.vals), " "))
		default:
			return command(f)
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
