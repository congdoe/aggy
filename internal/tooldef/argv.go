package tooldef

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Placeholder is the one placeholder an argument may hold: the path of
// the repo or worktree the session runs in, filled in at wiring.
const Placeholder = "{repo}"

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
		if strings.HasPrefix(s, "--exec-path") && strings.Contains(s, Placeholder) {
			return bad("%q would run programs from the repo", s)
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
// Linking resolves the folder and checks it again through safefs
// (cli.checkSkillStore), so a symlink cannot point a skill link at a
// credential.
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

var telemetryNameRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// Defence in depth for telemetry_env, which only built-ins have: a name
// must say what it is for, and never be one that changes how a package
// runner, resolver, loader, or shell behaves (#97 review).
var (
	telemetryWords    = []string{"TELEMETRY", "TRACK", "ANALYTICS", "OPTOUT", "OPT_OUT"}
	telemetryValues   = []string{"0", "1", "true", "false", "off", "no", "yes", "disabled"}
	telemetryPrefixes = []string{"NPM_CONFIG_", "PNPM_", "YARN_", "BUN_", "DENO_", "UV_", "PIP_", "PIPX_", "COREPACK_",
		"GO", "CARGO_", "RUSTUP_", "RUSTC_", "XDG_", "SSL_", "DOTNET_", "ELECTRON_", "LUA_"}
	telemetryNever = []string{"HOSTALIASES", "LOCALDOMAIN", "RES_OPTIONS", "GCONV_PATH", "ZDOTDIR", "BASHOPTS",
		"SHELLOPTS", "JDK_JAVA_OPTIONS", "DO_NOT_TRACK", "USER", "LOGNAME", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE"}
)

// telemetryEnv checks telemetry_env: a built-in's only (a user
// definition may not set variables), at most maxEnv NAME=VALUE entries,
// each name upper case, naming telemetry, listed once, and none that
// forbiddenEnv or the lists above refuse, with a plain on/off value.
func telemetryEnv(kvs []string, builtIn bool) error {
	if len(kvs) == 0 {
		return nil
	}
	if !builtIn {
		return &Error{Key: "telemetry_env", Msg: "only aggy's built-in tools set variables; aggy sets DO_NOT_TRACK=1 for every tool"}
	}
	if len(kvs) > maxEnv {
		return &Error{Key: "telemetry_env", Msg: fmt.Sprintf("at most %d entries", maxEnv)}
	}
	var seen []string
	for i, kv := range kvs {
		bad := func(msg string) error { return &Error{Key: fmt.Sprintf("telemetry_env[%d]", i), Msg: msg} }
		n, v, ok := strings.Cut(kv, "=")
		switch {
		case !ok || !telemetryNameRE.MatchString(n) || !slices.Contains(telemetryValues, strings.ToLower(v)):
			return bad("want NAME=VALUE: a name of A-Z, 0-9, and _, and a value of " + strings.Join(telemetryValues, ", "))
		case !slices.ContainsFunc(telemetryWords, func(w string) bool { return strings.Contains(n, w) }):
			return bad(n + " does not name a telemetry setting (" + strings.Join(telemetryWords, ", ") + ")")
		case slices.Contains(telemetryNever, n) || hasPrefixIn(n, telemetryPrefixes) || strings.HasSuffix(n, "_CA_BUNDLE"):
			return bad(n + " may not be set: aggy sets it, or it changes how programs fetch, resolve, load, or run")
		case slices.Contains(seen, n):
			return bad(n + " listed twice")
		}
		if why := forbiddenEnv(n); why != "" {
			return bad(n + " may not be set: " + why)
		}
		seen = append(seen, n)
	}
	return nil
}
