package hook

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/safefs"
)

// Events the OpenCode plugin sends with `aggy hook --vendor opencode
// <event>` (internal/adapter/opencode/plugin/server.js), besides the turn
// ends Stop, StopFailure, and StopCancelled, which it maps from
// session.execution.succeeded, failed, and interrupted.
const (
	EventGuardLoaded       = "GuardLoaded"
	EventPermissionAsked   = "PermissionAsked"
	EventPermissionReplied = "PermissionReplied"
	EventQuestionAsked     = "QuestionAsked"
	EventQuestionReplied   = "QuestionReplied"
)

// OpenCodeInput is what the shipped plugin hands the guard for one tool
// call (internal/adapter/opencode/plugin/server.js): the tool's name and
// input fields as they stand in core/src/tool/plugin at v2.2.5, the folder
// the plugin loaded for, and the launch's repo root.
type OpenCodeInput struct {
	Tool      string `json:"tool"`
	Directory string `json:"directory"`
	Root      string `json:"root"`
	Path      string `json:"path"`
	Command   string `json:"command"`
	Workdir   string `json:"workdir"`
	PatchText string `json:"patchText"`
	// Pattern is read for glob only: grep's is a regular expression.
	Pattern string `json:"pattern"`
	// Auto says the launch used full auto, from the plugin's options, which
	// come from aggy's per-launch config and not from the tool call.
	Auto bool `json:"auto"`
	// Always says the plugin saw a permission answered with Always allow this
	// session, so a rule outside the rendered list is in force.
	Always bool `json:"always"`
	// Strict says this launch's strict groups ask rather than deny
	// (parity.OpenCodeConfig's strictAsk), from the plugin's options: the
	// guard then refuses a strict match nothing cleared (PR #129 review).
	Strict bool `json:"strict"`
	// Evaluating says the plugin's permission evaluate hook was registered in
	// this OpenCode process; without it nothing can have cleared a call.
	Evaluating bool `json:"evaluating"`
	// Cleared are the resources of this tool's action that went to a prompt,
	// or that aggy pre-approved, in this process, recently.
	Cleared []string `json:"cleared"`
	// Plan and Sum are the launch's decision plan and its SHA-256, for the
	// strict groups and the default branches.
	Plan string `json:"plan"`
	Sum  string `json:"sum"`
}

// OpenCodeEnv is what the guard reads from outside the payload.
type OpenCodeEnv struct {
	// Real resolves an existing path, following links. Nil means
	// safefs.RealPath, which also refuses a vendor credential file.
	Real func(string) (string, error)
	// Lstat tells a missing path. Nil means safefs.Lstat.
	Lstat func(string) (fs.FileInfo, error)
	// Getenv finds the home a leading ~ means, and OpenCode's data folder for
	// its own saved tool output.
	Getenv func(string) string
	// DeniedNames are the file names to refuse in any case
	// (parity.OpenCodeDeniedNames, which this package cannot import).
	DeniedNames []string
	// ReadableNames are exempt from DeniedNames, exactly and whatever their
	// case: .env templates meant to be read (parity.EnvTemplates), which the
	// .env.* name would otherwise catch.
	ReadableNames []string
	// ShellRules are the rendered shell allow rules (parity.OpenCodeShellRules).
	// None means the scan always runs.
	ShellRules []string
	// ScanAlways runs the scan whatever a command holds: the person's rules are
	// unknown, as a file could not be read. ShellRules still names the functions
	// to look for, so it must keep at least the rules aggy ships.
	ScanAlways bool
	// AlwaysSeen says this repo has had a permission answered with Always allow
	// under aggy, in any session. Nil means no.
	AlwaysSeen func(root string) bool
	// StrictShell reports a shell command a strict policy group matches, by
	// the launch's plan (plan, sum); where the plan cannot be read, any policy
	// match counts. Nil counts every command as strict.
	StrictShell func(plan, sum, cmd string) bool
	// KeyNames are the private key file names the read tool may open, after a
	// prompt, on a strict launch (parity.OpenCodeKeyFiles).
	KeyNames []string
}

// The refusals' words. They go to the model, as the tool's error.
const (
	whyBadPayload = "aggy's guard could not read this call, so it was refused."
	whyDeniedName = "aggy's guard refuses this file by its name: it may hold a secret, whatever its case."
	whyAuthExport = "aggy's guard refuses `opencode auth export`: it prints sign-in secrets."
	whyFolder     = "aggy's guard refuses a tool call from another folder than the one this session was launched in: that project's settings and trust were not checked."
	whyOutside    = "aggy's guard refuses a path outside this repo: its real location is elsewhere, or a link points out of the repo. Ask the person to copy it in, if it is needed."
	whyDisguised  = "aggy's guard refuses this file: its name looks harmless, but it is a link, or passes through one, to a file the safety policy guards, and a prompt would show the harmless name."
	whyUncleared  = "aggy's guard refuses this call: it matches a strict safety policy group (a force push to a protected branch, or reading a private key), and neither a prompt you answered nor an exact pre-approval cleared it, as aggy's permission check did not run for it. Run it on its own so OpenCode asks, or run it yourself."
)

// authExportRE matches the command by its words, in any spacing. It sees
// the raw command, so a variable or a script gets past it (docs/design.md).
var authExportRE = regexp.MustCompile(`\bopencode\s+(?:[^\n;&|]*\s)?auth\s+export\b`)

// patchFileRE finds the files a patch tool's text names. OpenCode trims each
// line before it reads a header (util/src/patch.ts at v2.2.5), so any spacing
// counts here, and a spelling it would reject still has its path checked.
var patchFileRE = regexp.MustCompile(`(?m)^\s*\*\*\*\s*(?:Add File|Update File|Delete File|Move to)\s*:[ \t]*(.*?)\s*$`)

// globOutRE is a glob pattern that leaves its folder. OpenCode gives the
// pattern to its file search as it stands (core/src/tool/plugin/glob.ts).
var globOutRE = regexp.MustCompile(`(^|[{,])[/~]|(^|[/{,])\.\.($|[/},])`)

// OpenCodeGuard decides one tool call. It catches mistakes, not a hostile
// model: it reads the command's text, so a variable, a glob, a script, or
// base64 gets past it. Anything it cannot read or resolve is refused.
func OpenCodeGuard(payload []byte, env OpenCodeEnv) Decision {
	var in OpenCodeInput
	if err := json.Unmarshal(payload, &in); err != nil || in.Tool == "" || !filepath.IsAbs(in.Directory) || !filepath.IsAbs(in.Root) {
		return Decision{Deny: true, Reason: whyBadPayload}
	}
	if env.Real == nil {
		env.Real = safefs.RealPath
	}
	if env.Lstat == nil {
		env.Lstat = safefs.Lstat
	}
	root, err := env.Real(in.Root)
	if err != nil {
		return Decision{Deny: true, Reason: whyOutside}
	}
	// RealPath takes a name that does not exist yet. A root must exist.
	if _, err := env.Lstat(root); err != nil {
		return Decision{Deny: true, Reason: whyOutside}
	}
	dir, err := env.Real(in.Directory)
	if err != nil || !within(dir, root) {
		// The full screen's /open moves a session to another project, with
		// the first one's settings; a command there ran with no prompt
		// under full auto (fake model).
		return Decision{Deny: true, Reason: whyFolder}
	}
	inside := func(p string) bool {
		r, err := realTarget(p, dir, env)
		return err == nil && (within(r, root) || ownToolOutput(in.Tool, r, env))
	}
	strict := in.Strict && !in.Auto
	used := 0
	// Any tool's path counts, so a tool new in a later OpenCode is covered.
	if in.Path != "" {
		if !inside(in.Path) {
			return Decision{Deny: true, Reason: whyOutside}
		}
		plain := plainFile(in.Path, dir, env)
		readable := env.ReadableNames
		if !plain {
			// A template name on a link or a second hard link may be .env
			// itself, so it gets no exemption.
			readable = nil
		}
		// A harmless name that leads to a guarded file (owner decision,
		// 2026-11-10): OpenCode's prompt would show the harmless name.
		if r, err := realTarget(in.Path, dir, env); err != nil || deniedName(r, env.DeniedNames, readable) && !deniedName(in.Path, env.DeniedNames, readable) {
			return Decision{Deny: true, Reason: whyDisguised}
		}
		if deniedName(in.Path, env.DeniedNames, readable) {
			// On a strict launch the read tool may open a private key by its
			// own name, asked: the path as given is the file itself, with no
			// link on the way and no second name, and a prompt or an exact
			// pre-approval cleared it (owner decision, 2026-11-10).
			if !strict || in.Tool != "read" || !plain || !deniedName(in.Path, env.KeyNames, nil) || !direct(in.Path, dir, env) {
				return Decision{Deny: true, Reason: whyDeniedName}
			}
			if used = clearedPath(in, dir, env); used == 0 {
				return Decision{Deny: true, Reason: whyUncleared}
			}
		}
	}
	switch in.Tool {
	case "shell":
		switch {
		case authExportRE.MatchString(in.Command):
			return Decision{Deny: true, Reason: whyAuthExport}
		case in.Workdir != "" && !inside(in.Workdir):
			return Decision{Deny: true, Reason: whyOutside}
		}
		// A strict match runs only if a prompt or an exact pre-approval
		// cleared this very command (PR #129 review, HIGH-2): a saved Always
		// allow, a failing evaluate hook, or none at all, runs nothing strict.
		if strict && (env.StrictShell == nil || env.StrictShell(in.Plan, in.Sum, in.Command)) {
			i := slices.Index(in.Cleared, in.Command)
			if !in.Evaluating || i < 0 {
				return Decision{Deny: true, Reason: whyUncleared}
			}
			used = i + 1
		}
		// Full auto runs any command with no prompt, so what the scan protects
		// is not there to protect (docs/design.md).
		if !in.Auto {
			always := in.Always || env.ScanAlways || (env.AlwaysSeen != nil && env.AlwaysSeen(root))
			if why := shellScan(in.Command, env.ShellRules, always); why != "" {
				return Decision{Deny: true, Reason: why}
			}
		}
	case "patch", "apply_patch":
		for _, m := range patchFileRE.FindAllStringSubmatch(in.PatchText, -1) {
			if !inside(m[1]) {
				return Decision{Deny: true, Reason: whyOutside}
			}
		}
	case "glob":
		if globOutRE.MatchString(in.Pattern) {
			return Decision{Deny: true, Reason: whyOutside}
		}
	}
	return Decision{Cleared: used}
}

// direct reports a path that leads where its text says, from base: no link
// at any part of it, so the real path is the cleaned path.
func direct(p, base string, env OpenCodeEnv) bool {
	q, err := expandHome(p, env)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(q) {
		q = filepath.Join(base, q)
	}
	r, err := realTarget(p, base, env)
	return err == nil && r == filepath.Clean(q)
}

// clearedPath is 1 + the index of the clear naming in.Path, a resource of a
// read that went to a prompt or was pre-approved, compared from the session's
// folder; 0 when none does or the evaluate hook is not there.
func clearedPath(in OpenCodeInput, dir string, env OpenCodeEnv) int {
	if !in.Evaluating {
		return 0
	}
	abs := func(p string) string {
		q, err := expandHome(p, env)
		if err != nil {
			return ""
		}
		if !filepath.IsAbs(q) {
			q = filepath.Join(dir, q)
		}
		return filepath.Clean(q)
	}
	want := abs(in.Path)
	for i, c := range in.Cleared {
		if want != "" && abs(c) == want {
			return i + 1
		}
	}
	return 0
}

// within reports whether p is dir or under it. Both are real paths.
func within(p, dir string) bool {
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, "/")+"/")
}

// realTarget is where p really leads from base, walked one name at a time,
// so a link anywhere on the way counts, and `..` is taken from the real
// folder rather than cancelled in the text. A name that does not exist yet
// (a file about to be written) stays as it is.
func realTarget(p, base string, env OpenCodeEnv) (string, error) {
	p, err := expandHome(p, env)
	if err != nil {
		return "", err
	}
	cur := base
	if filepath.IsAbs(p) {
		cur = "/"
	}
	for _, name := range strings.Split(p, "/") {
		switch name {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		switch _, err := env.Lstat(next); {
		case errors.Is(err, fs.ErrNotExist):
			cur = next
		case err != nil:
			return "", err
		default:
			r, err := env.Real(next)
			if err != nil {
				return "", err
			}
			cur = r
		}
	}
	return cur, nil
}

// expandHome does what OpenCode does to a path before it uses it: ~ and a
// leading ~/ mean the home (resolvePath, core/src/file-access.ts at v2.2.5).
// It takes ~user as a file of that name, but a shell would not, so that is
// refused, as a home it cannot find is.
func expandHome(p string, env OpenCodeEnv) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return "", errors.New("a path starting with ~user")
	}
	home := ""
	if env.Getenv != nil {
		home = env.Getenv("HOME")
	}
	if !filepath.IsAbs(home) {
		return "", errors.New("no home to expand ~ with")
	}
	return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/")), nil
}

// plainFile reports that p, from base, names a regular file with one link,
// not a symlink, or nothing yet (a file about to be written). Anything it
// cannot check is not plain.
func plainFile(p, base string, env OpenCodeEnv) bool {
	p, err := expandHome(p, env)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	fi, err := env.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return true
	case err != nil:
		return false
	}
	return fi.Mode().IsRegular() && safefs.LinkCount(fi) <= 1
}

// deniedName reports a path whose file name the policy denies, whatever its
// case: a policy's rule matches exactly, and macOS folds case, so `.ENV` is
// `.env` there. It matches the name given, and grep's search text is not
// covered at all (docs/design.md).
func deniedName(p string, names, readable []string) bool {
	base := strings.ToLower(filepath.Base(p))
	for _, n := range readable {
		if base == strings.ToLower(n) {
			return false
		}
	}
	for _, n := range names {
		if ok, err := filepath.Match(strings.ToLower(n), base); err == nil && ok {
			return true
		}
	}
	return false
}

// ownToolOutput reports OpenCode's saved copy of a long tool result, which
// the model is told to read (core/src/tool-output.ts at v2.2.5): under
// its data folder's tool-output, by read or grep only.
func ownToolOutput(tool, real string, env OpenCodeEnv) bool {
	if env.Getenv == nil || (tool != "read" && tool != "grep") {
		return false
	}
	data := env.Getenv("XDG_DATA_HOME")
	if data == "" {
		h := env.Getenv("HOME")
		if h == "" {
			return false
		}
		data = filepath.Join(h, ".local", "share")
	}
	dir := filepath.Join(data, "opencode", "tool-output")
	if r, err := env.Real(dir); err == nil {
		dir = r
	}
	return within(real, dir) && real != dir
}

// sensitiveVars are the variables that change what the shipped whitelist runs
// (parity.OpenCodeStarter: git status, diff, log, and branch, ls, and pwd), or
// where the session's shell starts. A trailing * is a prefix.
//
// Not listed: GOOS and CGO_ENABLED, and the language tools' GOFLAGS,
// NODE_OPTIONS, PYTHON*, PERL5*, and RUBY*. No shipped rule runs those tools.
// One could reach them only through a repo's hook or filter in that language,
// which already runs whatever it says, and `PYTHONPATH=. pytest` is everyday.
// IFS: shells split only expansions, and no shipped rule holds one, so it cannot
// change an exact rule's command (TestGuardAgainstRealShells), and `while IFS=
// read -r line` is among the commonest loops there are. A rule with an expansion
// would need it back.
var sensitiveVars = []string{
	"PATH",                // which git or ls runs at all
	"GIT_*",               // git's config, repo, and helpers: GIT_CONFIG_COUNT, GIT_DIR, GIT_EXTERNAL_DIFF, GIT_PAGER
	"LD_*",                // loads a library into any program, on Linux
	"DYLD_*",              // the same on macOS
	"GCONV_PATH",          // loads a module into a glibc program that converts text, as git log can
	"BASH_*",              // BASH_ENV, a file each bash script runs first; BASH_FUNC_x%%, an imported function; BASH_CMDS, the command hash
	"ENV",                 // the same file as BASH_ENV, for sh
	"DEVELOPER_DIR",       // on macOS /usr/bin/git is a shim, which runs the xcrun under this folder
	"TOOLCHAINS",          // which toolchain the same shim's xcrun looks in
	"PS4",                 // text a shell expands when it traces, which SHELLOPTS can turn on in a child
	"SHELLOPTS",           // bash options a child bash starts with
	"BASHOPTS",            // the same
	"ZDOTDIR",             // where zsh finds its startup files
	"HOME",                // ~/.gitconfig, so core.pager, aliases, and filters
	"XDG_*",               // $XDG_CONFIG_HOME/git/config, the same
	"CDPATH",              // which folder a cd before the command lands in
	"FPATH",               // zsh's functions, tied to fpath
	"MANPATH",             // tied to manpath, so a rule below covers both spellings
	"SHELL",               // the shell programs start commands with
	"PAGER",               // the program git log and diff pipe through
	"MANPAGER",            // the same for man, which git help runs
	"LESSOPEN",            // a command less runs on each file, when it is the pager
	"LESSCLOSE",           // the same, on closing
	"LESS",                // less's own flags, which can run a command
	"LESSKEY",             // a file of its key bindings and settings
	"EDITOR",              // what git starts for a message
	"VISUAL",              // the same
	"RIPGREP_CONFIG_PATH", // flags rg reads from a file, where a rule names rg
}

// tiedVars are zsh's lower-case arrays tied to PATH and its like, and the
// zsh/parameter arrays that hold its hash, functions, and aliases. They are
// common words, so only a shape that sets one counts.
var tiedVars = []string{"path", "cdpath", "fpath", "manpath", "commands", "functions", "aliases"}

var (
	// quoteMarksRE is what the shell removes from a word once it has read it.
	quoteMarksRE = regexp.MustCompile(`["'\\]`)
	// shellWordRE finds each run of name characters in the raw text.
	shellWordRE = regexp.MustCompile(`[A-Za-z0-9_]+`)
	// bracedSetRE is what follows a name in ${NAME:=x}, ${NAME=x}, ${NAME::=x}
	// (zsh), and each of them with a subscript: expansions that set it.
	bracedSetRE = regexp.MustCompile(`^(?:\[[^\]\n]*\])?:{0,2}=`)
	// tiedSetRE is what follows a tied name where it is set.
	tiedSetRE = regexp.MustCompile(`^(?:\+?=|:{1,2}=|\[)`)
	// loopWordsRE is a loop's or read's words, up to the end of the statement.
	loopWordsRE = regexp.MustCompile(`(?:^|[\s;&|(){}])(?:for|select|foreach|read)[ \t]+([^;&|()<>\n]*)`)
	// declWordRE is a declaration builtin and the rest of its statement.
	declWordRE = regexp.MustCompile(`(?:^|[\s;&|(){}!])(?:declare|typeset|export|readonly|local|integer|float|unset|unsetenv)[ \t]+([^;&|\n]*)`)
)

// A byte before a tied name that makes it part of a longer word, as in
// --path=x, a/path=x, or k=v,path=x, so not a variable being set.
const tiedNotSetAfter = "-./:=@%+,~?"

// shellScan says why the guard refuses cmd, or "". OpenCode's permission
// check gives some statements no resource to ask about (core/src/shell/scan.ts
// at v2.2.5): a bare assignment anywhere, a declaration word, a for or select
// variable, and (( )). So `for PATH in ./bin; do git status; done` ran ./bin/git
// with no prompt under an allowed `git status`.
//
// That matters only when every command left is allowed, so nothing prompts. If
// the text holds no allow rule, OpenCode asks, and the person sees the command:
// a refusal there would only replace a prompt they could answer. So the scan
// runs when the text holds a rule (holdsRule), when always says a rule outside
// the rendered list is in force, or when there is no list to go by.
//
// This does not parse shell, and it knows nothing of quotes: parsing was
// bypassed in each review, and refused everyday commands. It looks at the raw
// text, so it refuses a commit message that names PATH beside an allowed
// command, and it allows any other declaration. It reads three forms, and any
// one can refuse. A line continuation is the one thing the shell removes before
// anything else, so the second has those gone: `PA\<newline>TH=x` sets PATH, and
// `# note\<newline>PATH=x` only does as raw text. The third also has quotes and
// backslashes gone, as `export P"A"TH=x` sets PATH with no prompt.
//
// It catches mistakes, not a hostile model: a name assembled at run time gets
// past it (docs/design.md).
//
// The zsh form `git foo() ./bin/git`, a function with two names and no braces,
// is not refused here, since telling it from a commit message needs parsing.
// OpenCode's scanner cannot analyze it, and that fails the call (scan.ts and
// shell/parse.ts at v2.2.5).
//
// TODO(T6953): the review's remaining shapes, and an Always allow given outside
// aggy, which is saved per project in opencode.db, so aggy cannot see it.
func shellScan(cmd string, rules []string, always bool) string {
	joined := strings.ReplaceAll(cmd, "\\\n", "")
	forms := []string{cmd, joined, quoteMarksRE.ReplaceAllString(joined, "")}
	if !always && len(rules) > 0 && !slices.ContainsFunc(forms, func(f string) bool { return holdsRule(f, rules) }) {
		return ""
	}
	for _, f := range forms {
		if why := scanText(f, firstWords(rules)); why != "" {
			return why
		}
	}
	return ""
}

// holdsRule reports text that holds an allow rule as words, with its whitespace
// collapsed. OpenCode needs far less: a resource must be the rule exactly, as
// its Wildcard.match has no whitespace rule and rawWords are not used for the
// match (permission.ts and util/wildcard.ts at v2.2.5). So `\git status`,
// `command git status`, and `/usr/bin/git status` all ask, but they count here,
// since over-matching only runs the scan.
func holdsRule(text string, rules []string) bool {
	text = strings.Join(strings.Fields(text), " ")
	wordByte := func(c byte) bool {
		return c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
	}
	for _, r := range rules {
		// A rule is compared collapsed too, or two spaces in it would never
		// be found in the text, and the gate would pass over the scan.
		r = strings.Join(strings.Fields(r), " ")
		if r == "" {
			continue
		}
		for from := 0; ; {
			i := strings.Index(text[from:], r)
			if i < 0 {
				break
			}
			i += from
			end := i + len(r)
			if (i == 0 || !wordByte(text[i-1])) && (end == len(text) || !wordByte(text[end])) {
				return true
			}
			from = i + 1
		}
	}
	return false
}

// firstWords are the rules' first words, once each: a function by one of these
// names would run in place of an allowed command.
func firstWords(rules []string) []string {
	var out []string
	for _, r := range rules {
		if w, _, _ := strings.Cut(r, " "); !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	return out
}

// scanText is one pass over the text.
func scanText(text string, ruleWords []string) string {
	for _, loc := range shellWordRE.FindAllStringIndex(text, -1) {
		word, before, after := text[loc[0]:loc[1]], text[:loc[0]], text[loc[1]:]
		tied := slices.Contains(tiedVars, word)
		if !tied && !sensitiveVar(word) {
			continue
		}
		if readsVar(before, after) {
			continue
		}
		if tied && (!tiedSetRE.MatchString(after) || (before != "" && strings.IndexByte(tiedNotSetAfter, before[len(before)-1]) >= 0)) {
			continue
		}
		return whyVariable(word)
	}
	if name := tiedLoopVar(text); name != "" {
		return whyVariable(name)
	}
	for _, name := range ruleWords {
		if definesFunction(text, name) {
			return fmt.Sprintf("aggy's guard refuses this shell command: beside an allowed command, it looks like it defines a function named %s, which would run in its place, and OpenCode's permission check would not ask. Rewrite it without that text. "+guardRoutes, name)
		}
	}
	if dynamicDeclaration(text) {
		return "aggy's guard refuses this shell command: beside an allowed command, a declaration (declare, typeset, export, readonly, local, integer, float, or unset) takes its variable's name from a $ expansion, so aggy cannot tell which variable it sets. Rewrite it with the name spelled out. " + guardRoutes
	}
	return ""
}

// guardRoutes is the way round for the usual reasons to name one.
const guardRoutes = "For a pager use git --no-pager, for a commit message git commit -F, and for a search the grep tool."

// tiedLoopVar finds a tied name among a loop's or read's variables: any of
// its words, up to `in`. A flag is no name, nor is what follows `in`.
func tiedLoopVar(text string) string {
	for _, m := range loopWordsRE.FindAllStringSubmatch(text, -1) {
		for _, f := range strings.Fields(m[1]) {
			if f == "in" {
				break
			}
			if slices.Contains(tiedVars, f) {
				return f
			}
		}
	}
	return ""
}

// whyVariable is the refusal that names the variable.
func whyVariable(name string) string {
	return fmt.Sprintf("aggy's guard refuses this shell command: beside an allowed command, it names %s, which can change what that command runs, and OpenCode's permission check does not see a variable being set. Reading it as $%s is fine; any other mention counts, in a comment or a commit message too. Run the allowed command on its own, or rewrite without the name. "+guardRoutes, name, name)
}

// sensitiveVar reports a word in sensitiveVars.
func sensitiveVar(word string) bool {
	for _, n := range sensitiveVars {
		if p, ok := strings.CutSuffix(n, "*"); ok {
			if strings.HasPrefix(word, p) {
				return true
			}
		} else if word == n {
			return true
		}
	}
	return false
}

// readsVar reports an appearance that reads the variable: $NAME, or ${NAME
// and ${#NAME and ${!NAME unless the expansion sets it.
func readsVar(before, after string) bool {
	if strings.HasSuffix(before, "$") {
		return true
	}
	for _, open := range []string{"${", "${#", "${!"} {
		if strings.HasSuffix(before, open) {
			return !bracedSetRE.MatchString(after)
		}
	}
	return false
}

// definesFunction reports text that looks like it defines a function called
// name: name(), name (), function name, and zsh's forms with several names,
// `name other() {` and `function other name {`. A name in quotes or after a
// backslash counts, as the shells take those too.
func definesFunction(text, name string) bool {
	q := regexp.QuoteMeta(name)
	const start = `(?:^|[\s;&|(){}` + "`" + `])`
	const word = `[^\s;&|(){}]+`
	for _, pattern := range []string{
		start + `["'\\]?` + q + `["']?\s*\(\s*\)`,
		start + `function\s+(?:-\S+\s+)*["'\\]?` + q + `(?:[\s;&|(){}"']|$)`,
		start + q + `(?:[ \t]+` + word + `)+[ \t]*\(\s*\)\s*[{(]`,
		start + `function(?:[ \t]+` + word + `)+[ \t]+` + q + `\b[^;&|\n]*[{(]`,
	} {
		if regexp.MustCompile(pattern).MatchString(text) {
			return true
		}
	}
	return false
}

// dynamicDeclaration reports a declaration builtin with a $ or a backquote
// where a name goes: in any word's text before its =, or at the start of a word
// with no =, which may expand to a whole assignment, except that in the first
// word it counts anywhere. A value may hold either: `export A=$X B=$Y` passes.
func dynamicDeclaration(text string) bool {
	for _, m := range declWordRE.FindAllStringSubmatch(text, -1) {
		first := true
		for _, f := range strings.Fields(m[1]) {
			if first && (f[0] == '-' || f[0] == '+') {
				continue
			}
			name, _, hasValue := strings.Cut(f, "=")
			trimmed := strings.TrimLeft(f, `"'`)
			switch {
			case (first || hasValue) && strings.ContainsAny(name, "$`"):
				return true
			case !hasValue && trimmed != "" && strings.ContainsAny(trimmed[:1], "$`"):
				return true
			}
			first = false
		}
	}
	return false
}
