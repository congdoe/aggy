package tooldef

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"

	"github.com/congdoe/aggy/internal/hook"
)

// Placeholder is the one placeholder an argument may hold: the path of
// the repo or worktree the session runs in, filled in at wiring.
const Placeholder = "{repo}"

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
		// (sed's e command, awk's system(), vim -c, lldb -o, gdb -ex),
		// and none is a tool's program.
		"sed", "gsed", "awk", "gawk", "mawk", "nawk", "vi", "vim", "nvim", "view", "ex", "lldb", "gdb"}
	// interpreters run code given with -c, -e, -E, --eval, or eval.
	interpreters = []string{"python", "perl", "ruby", "node", "nodejs", "php", "lua", "luajit", "osascript",
		"tclsh", "wish", "rscript", "julia", "deno", "bun", "jshell", "irb", "groovy"}
	// fetchers download a package and run it: npx -y pkg@latest or uvx
	// pkg would run new code on every launch.
	fetchers = []string{"npx", "pnpx", "bunx", "uvx"}
	// codeFlags give an interpreter its code inline.
	codeFlags = []string{"-c", "-e", "-E", "--eval", "--command", "--exec", "-r", "eval", "--print", "-p"}
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

// checkPrograms refuses a shell, env, or other command runner at the
// program, or anywhere after a wrapper (nice, timeout, xargs, npx, ...),
// and an interpreter given inline code.
func checkPrograms(a []string) string {
	for i := 0; i < len(a); i++ {
		n := normProgram(a[i])
		switch {
		case isShell(n):
			return fmt.Sprintf("%q is a shell, which would run a shell string", a[i])
		case slices.Contains(neverPrograms, n):
			return fmt.Sprintf("%q runs a string as a command, or as another user", a[i])
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
			continue
		case slices.Contains(fetchers, n):
			if why := checkFetch(a[i], a[i+1:]); why != "" {
				return why
			}
			continue
		case slices.Contains([]string{"npm", "pnpm", "yarn", "uv", "pipx"}, n):
			// npm exec, pnpm dlx, yarn dlx, uv run, uv tool run, pipx run.
			subs := []string{"exec", "dlx", "x"}
			if n == "uv" || n == "pipx" {
				subs = []string{"run"}
			}
			for k, f := range a[i+1:] {
				if slices.Contains(subs, f) {
					if why := checkFetch(a[i]+" "+f, a[i+2+k:]); why != "" {
						return why
					}
					break
				}
			}
			if i > 0 {
				continue
			}
			return ""
		case hook.IsWrapper(n):
			continue // what the wrapper runs is checked too
		case hasPrefixIn(n, interpreters):
			for _, f := range a[i+1:] {
				if codeFlag(f) {
					return fmt.Sprintf("%q is given inline code with %q", a[i], f)
				}
			}
			return ""
		}
		if i > 0 {
			continue // an argument after a wrapper: keep looking for a runner
		}
		return ""
	}
	return ""
}

// checkFetch refuses a package runner that would fetch new code on a
// launch, which breaks "never install unasked": a shell string (npx -c),
// -y or --yes, and a package not pinned to a version (pkg, pkg@latest).
func checkFetch(prog string, rest []string) string {
	named := false // a pinned --package or --from names the package
	for k := 0; k < len(rest); k++ {
		f := rest[k]
		name, val, hasVal := strings.Cut(f, "=")
		switch {
		case f == "-c" || name == "--call":
			return fmt.Sprintf("%q is given a shell string with %q", prog, f)
		case f == "-y" || name == "--yes":
			return fmt.Sprintf("%q with %q installs a package on launch without asking", prog, f)
		case name == "-p" || name == "--package" || name == "--from" || name == "--with":
			if !hasVal && k+1 < len(rest) {
				k++
				val = rest[k]
			}
			if !pinned(val) {
				return fmt.Sprintf("%q runs %q, which is not pinned to a version (want name@1.2.3)", prog, val)
			}
			named = named || name != "--with" // --with adds a dependency only
		case strings.HasPrefix(f, "-"):
		default:
			if !named && !pinned(f) {
				return fmt.Sprintf("%q runs %q, which is not pinned to a version (want name@1.2.3)", prog, f)
			}
			return ""
		}
	}
	return ""
}

// pinned reports whether a package spec names an exact version, not a
// tag: name@1.2.3, @scope/name@1.2.3-beta.1, or name==1.2.3.
func pinned(spec string) bool {
	v := ""
	if _, after, ok := strings.Cut(spec, "=="); ok {
		v = after
	} else if i := strings.LastIndexByte(spec, '@'); i > 0 {
		v = spec[i+1:]
	}
	return userVersionRE.MatchString(v)
}

// codeFlag reports an inline-code flag: one of codeFlags, its --x=value
// form, or a short cluster holding c, e, or E (python -Ic, perl -we).
func codeFlag(f string) bool {
	name, _, _ := strings.Cut(f, "=")
	if slices.Contains(codeFlags, name) {
		return true
	}
	return len(f) > 2 && f[0] == '-' && f[1] != '-' && strings.ContainsAny(f[1:], "ceE")
}

// argv checks a command: a program and its arguments, run directly. The
// program is a bare name looked up on PATH or a clean absolute path, and
// never a shell, env, or other runner of a string.
func argv(key string, a []string, required bool) error {
	bad := func(f string, x ...any) error { return &Error{Key: key, Msg: fmt.Sprintf(f, x...)} }
	if len(a) == 0 {
		if required {
			return bad("want a program and its arguments")
		}
		return nil
	}
	if len(a) > maxArgs {
		return bad("at most %d arguments", maxArgs)
	}
	for _, s := range a {
		if len(s) > maxArg {
			return bad("an argument is over %d bytes", maxArg)
		}
		if strings.ContainsFunc(s, isCtl) {
			return bad("an argument holds a control or format character")
		}
		if strings.ContainsAny(strings.ReplaceAll(s, Placeholder, ""), "{}") {
			return bad("%q holds a placeholder other than %s", s, Placeholder)
		}
	}
	p := a[0]
	switch {
	case strings.Contains(p, Placeholder):
		return bad("the program %q holds %s: the repo's own code would run as the tool", p, Placeholder)
	case strings.ContainsFunc(p, func(r rune) bool { return r > unicode.MaxASCII }):
		// A case-insensitive file system folds more than ASCII (/bin/ſh).
		return bad("the program %q must be ASCII", p)
	case p == "" || strings.ContainsFunc(p, unicode.IsSpace):
		return bad("the program %q must be one word: a command is argv, never a shell string", p)
	case strings.HasPrefix(p, "/"):
		if path.Clean(p) != p {
			return bad("the program %q must be a clean absolute path", p)
		}
	case strings.Contains(p, "/") || p == "." || p == "..":
		return bad("the program %q must be a bare name found on PATH or an absolute path", p)
	}
	if why := checkPrograms(a); why != "" {
		return bad("%s", why)
	}
	return nil
}

// hookChars are the characters a hook command's words may hold: every
// CLI hands the command to a shell (Claude sh -c, Codex $SHELL -lc, Grok
// inline; internal/hook/install.go), and none of these is special to
// one. It is CheckBin's set.
const hookChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._+-@"

// shellSafe refuses a hook word a shell would read as more than text,
// and {repo}: a repo path may hold any character, and Codex runs a hook
// through $SHELL, so no one quoting is right for sh and fish alike.
func shellSafe(key string, a []string) error {
	for _, s := range a {
		if strings.Contains(s, Placeholder) {
			return &Error{Key: key, Msg: fmt.Sprintf("%q: a hook command runs in a shell, which cannot be handed the repo's path safely, so it may not hold %s", s, Placeholder)}
		}
		if s == "" || strings.Trim(s, hookChars) != "" {
			return &Error{Key: key, Msg: fmt.Sprintf("%q: a hook command runs in a shell, so each word may hold only letters, digits, and / . _ + @ -", s)}
		}
	}
	return nil
}

// skillPath accepts a clean path under "~/". A user tool's skill lives
// in ~/.aggy/skills/<name>; a built-in's may be elsewhere under home,
// but not in a dot folder (vendor homes, ~/.ssh) or in ~/.aggy outside
// skills/.
// TODO(T6708): resolve the path and check it again through safefs when
// linking, so a symlink cannot point a skill link at a credential.
func skillPath(key, name, p string, builtIn bool) error {
	bad := func(m string) error { return &Error{Key: key, Msg: m} }
	if err := text(key, p, maxPath, true); err != nil {
		return err
	}
	rest, ok := strings.CutPrefix(p, "~/")
	if !ok || rest == "" || path.Clean(rest) != rest || rest == ".." || strings.HasPrefix(rest, "../") || strings.HasPrefix(rest, "/") {
		return bad("want a clean path under ~/")
	}
	own := "~/.aggy/skills/" + name
	if !builtIn {
		if p != own && !strings.HasPrefix(p, own+"/") {
			return bad("a user tool's skill must be under " + own)
		}
		return nil
	}
	first, _, _ := strings.Cut(rest, "/")
	if strings.HasPrefix(first, ".") && !strings.HasPrefix(rest, ".aggy/skills/") {
		return bad("want a path outside dot folders such as vendor homes, or under ~/.aggy/skills/")
	}
	return nil
}

// envRefused says why a variable may not be passed to a tool, or "". A
// user tool may pass only its own variables (<NAME>_...); every tool is
// refused base URLs, proxies, secrets, loader and runtime injection, and
// vendor CLIs' and aggy's own settings.
func envRefused(tool, n string, builtIn bool) string {
	if n == "" || len(n) > 64 || strings.Trim(n, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_") != "" || unicode.IsDigit(rune(n[0])) {
		return "want a variable name"
	}
	prefix := strings.ToUpper(strings.ReplaceAll(tool, "-", "_")) + "_"
	if !builtIn && !strings.HasPrefix(n, prefix) {
		return fmt.Sprintf("a user tool may pass only its own variables, starting %s", prefix)
	}
	if why := forbiddenEnv(n); why != "" {
		return n + " may not be passed: " + why
	}
	return ""
}

func forbiddenEnv(n string) string {
	u := strings.ToUpper(n)
	words := strings.Split(u, "_")
	hasWord := func(ws ...string) bool {
		return slices.ContainsFunc(words, func(w string) bool { return slices.Contains(ws, w) })
	}
	hasPart := func(ps ...string) bool {
		return slices.ContainsFunc(ps, func(p string) bool { return strings.Contains(u, p) })
	}
	switch {
	case strings.HasSuffix(u, "BASE_URL"):
		return "a base-URL variable can send traffic to another endpoint"
	case hasPart("PROXY"):
		return "aggy sets up no proxy"
	case hasWord("KEY", "KEYS", "PAT", "DSN", "PWD", "SK", "PEM", "SESSION", "COOKIE", "COOKIES", "PRIVATE", "CERT"),
		hasPart("APIKEY", "TOKEN", "SECRET", "PASS", "CREDENTIAL", "AUTH", "JWT", "COOKIE", "KUBECONFIG"):
		return "it names a key, secret, or credential"
	case hasPrefixIn(u, []string{"LD_", "DYLD_", "NODE_", "PYTHON", "PERL", "RUBY", "BASH_", "JAVA_TOOL_OPTIONS", "_JAVA_OPTIONS", "GIT_"}),
		slices.Contains([]string{"ENV", "SHELL", "PROMPT_COMMAND", "IFS", "PATH", "HOME"}, u):
		return "it changes how programs load or run"
	case hasPrefixIn(u, []string{"ANTHROPIC", "OPENAI", "XAI_", "GROK", "CODEX", "CLAUDE", "AGGY", "AWS_", "GOOGLE_", "AZURE_"}):
		return "it is a vendor CLI's, a cloud's, or aggy's own setting"
	}
	return ""
}
