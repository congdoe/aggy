package hook

import (
	"encoding/json"
	"errors"
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
}

// The refusals' words. They go to the model, as the tool's error.
const (
	whyBadPayload  = "aggy's guard could not read this call, so it was refused."
	whyDeclaration = "aggy's guard refuses a variable declaration in a shell command (OpenCode's permission check skips them, so one before an allowed command could change what it runs). Pass the value on the command it is for, or use a script the person can read."
	whyUnsure      = "aggy's guard cannot follow this command's shell syntax exactly, and it looks like it sets a variable or defines a function, which OpenCode's permission check does not see. Use plain commands; to write a file, use the edit tool rather than a heredoc."
	whyDeniedName  = "aggy's guard refuses this file by its name: it may hold a secret, whatever its case."
	whyAuthExport  = "aggy's guard refuses `opencode auth export`: it prints sign-in secrets."
	whyFolder      = "aggy's guard refuses a tool call from another folder than the one this session was launched in: that project's settings and trust were not checked."
	whyOutside     = "aggy's guard refuses a path outside this repo: its real location is elsewhere, or a link points out of the repo. Ask the person to copy it in, if it is needed."
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
	// Any tool's path counts, so a tool new in a later OpenCode is covered.
	if in.Path != "" {
		if !inside(in.Path) {
			return Decision{Deny: true, Reason: whyOutside}
		}
		if deniedName(in.Path, env.DeniedNames) {
			return Decision{Deny: true, Reason: whyDeniedName}
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
		if why := shellRefusal(in.Command); why != "" {
			return Decision{Deny: true, Reason: why}
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
	return Decision{}
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

// deniedName reports a path whose file name the policy denies, whatever its
// case: a policy's rule matches exactly, and macOS folds case, so `.ENV` is
// `.env` there. It matches the name given, and grep's search text is not
// covered at all (docs/design.md).
func deniedName(p string, names []string) bool {
	base := strings.ToLower(filepath.Base(p))
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

// declarations are the words OpenCode's shell scanner skips as permission
// resources (core/src/shell/scan.ts at v2.2.5, BASH_DECLARATIONS).
var declarations = []string{"declare", "typeset", "export", "readonly", "local", "unset", "unsetenv"}

// assignmentRE is a word that sets a variable.
var assignmentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\[[^\]]*\])?\+?=`)

// statementWords may come before a statement's own first word.
var statementWords = []string{"then", "elif", "else", "do", "!", "time", "command", "builtin"}

// compoundWords start syntax the parser below does not follow, at a
// statement's head.
var compoundWords = []string{"if", "for", "case", "select", "while", "until", "function", "repeat", "foreach", "coproc"}

// unsureText is raw text that means the parser below may lose its place, or
// that hides what it runs inside what looks like a word: heredocs, $”, $"",
// arithmetic, [[, ${, and command substitution, in or out of quotes.
var unsureText = []string{"<<", "$'", `$"`, "((", "[[", "${", "$(", "`"}

// The whole-text scan, for a command the parser cannot be sure of. Each
// ignores quotes on purpose, so it refuses some harmless text.
var (
	// A name and `=`, `+=`, or `:=`, with no word character before it, so
	// `--flag=x`, `$x=`, and `a/b=c` do not count.
	anyAssignRE = regexp.MustCompile(`(?:^|[^\w\-./:=$@%+,~])[A-Za-z_]\w*(?:\[[^\]\n]*\])?\+?:?=`)
	anyDeclRE   = regexp.MustCompile(`(?:^|[\s;&|(){}` + "`" + `"'!])(?:declare|typeset|export|readonly|local|unset|unsetenv)(?:\s|;|$)`)
	// Inside (( )) a name may be set with spaces, and by any operator.
	anyArithRE = regexp.MustCompile(`[A-Za-z_]\w*(?:\[[^\]\n]*\])?\s*(?:[-+*/%&|^]|<<|>>)?=(?:[^=]|$)`)
	anyStepRE  = regexp.MustCompile(`(?s)\(\(.*(?:\+\+|--)`)
	// name() or `function name`: a definition shadows the command it names.
	anyFuncRE = regexp.MustCompile(`(?:^|[\s;&|(){}])(?:[^\s;&|(){}'"$]+[ \t]*\([ \t]*\)|function[ \t])`)
	anyLoopRE = regexp.MustCompile(`(?:^|[\s;&|(){}])(?:for|select|foreach)[ \t]+([^;\n(]*)`)
)

// plainLoopVarRE is a loop variable that cannot be one a program reads, by
// the usual naming: environment variables are upper case.
var plainLoopVarRE = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// lowerEnvNames are lower-case names that do change a program: zsh ties these
// to PATH and its like, and git and curl read the proxy ones.
var lowerEnvNames = []string{"path", "cdpath", "fpath", "manpath", "mailpath", "module_path", "psvar", "fignore", "watch", "argv"}

// shellRefusal says why the guard refuses cmd, or "". OpenCode's permission
// check gives some statements no resource to ask about
// (core/src/shell/scan.ts at v2.2.5): a bare assignment anywhere, a declaration
// word, a for or select variable, and (( )). So `for PATH in ./bin; do git
// status; done` ran ./bin/git with no prompt under an allowed `git status`.
//
// A plain command is read word by word, so `git commit -m "a=b"` passes. Only
// syntax it follows exactly gets that: anything else is scanned as raw text, and
// refused if anything in it looks like it sets a variable or defines a function.
// `read` and `printf -v` also set one, but they are resources, so they ask.
//
// It catches mistakes, not a hostile model: a variable, a script, or base64
// gets past it (docs/design.md).
func shellRefusal(cmd string) string {
	cmd = strings.ReplaceAll(cmd, "\\\n", "") // the shell removes these first
	stmts, sure := statements(cmd)
	for _, t := range unsureText {
		sure = sure && !strings.Contains(cmd, t)
	}
	for _, words := range stmts {
		if head := statementHead(words); head < len(words) && slices.Contains(compoundWords, words[head]) {
			sure = false
		}
	}
	if !sure {
		if setsAnything(cmd) {
			return whyUnsure
		}
		return ""
	}
	for _, words := range stmts {
		if onlyDeclares(words) {
			return whyDeclaration
		}
	}
	return ""
}

// setsAnything is the raw-text scan.
func setsAnything(cmd string) bool {
	if anyAssignRE.MatchString(cmd) || anyDeclRE.MatchString(cmd) || anyFuncRE.MatchString(cmd) {
		return true
	}
	if strings.Contains(cmd, "((") && (anyArithRE.MatchString(cmd) || anyStepRE.MatchString(cmd)) {
		return true
	}
	for _, m := range anyLoopRE.FindAllStringSubmatch(cmd, -1) {
		for _, name := range strings.Fields(m[1]) {
			if name == "in" {
				break
			}
			if !plainLoopVarRE.MatchString(name) || slices.Contains(lowerEnvNames, name) || strings.HasSuffix(name, "_proxy") {
				return true
			}
		}
	}
	return false
}

// statementHead is the index of a statement's own first word.
func statementHead(words []string) int {
	i := 0
	for i < len(words) && slices.Contains(statementWords, words[i]) {
		i++
	}
	return i
}

// onlyDeclares reports a statement that only declares. `FOO=1 git status`
// does not count: it is one command, which the permission check sees whole,
// and it prompts.
func onlyDeclares(words []string) bool {
	i := statementHead(words)
	j := i
	for j < len(words) && assignmentRE.MatchString(words[j]) {
		j++
	}
	switch {
	case j < len(words):
		return slices.Contains(declarations, words[j])
	default:
		return j > i
	}
}

// statements splits a plain shell command into the words of each statement,
// and says whether it could follow it all. It only has to find where
// statements begin, in text with none of unsureText. Quoted text keeps its
// place in a word with its quotes gone, so `"X=1"` is not told from X=1: that
// errs towards refusing. An unterminated quote and `()` are not sure.
func statements(cmd string) ([][]string, bool) {
	var (
		out    [][]string
		words  []string
		word   strings.Builder
		inWord bool
		sure   = true
	)
	endWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endStatement := func() {
		endWord()
		if len(words) > 0 {
			out = append(out, words)
			words = nil
		}
	}
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		switch c {
		case '\\':
			i++
			if i < len(cmd) && cmd[i] != '\n' {
				word.WriteByte(cmd[i])
				inWord = true
			}
		case '\'':
			inWord = true
			j := strings.IndexByte(cmd[i+1:], '\'')
			if j < 0 {
				word.WriteString(cmd[i+1:])
				i = len(cmd)
				sure = false
				break
			}
			word.WriteString(cmd[i+1 : i+1+j])
			i += j + 1
		case '"':
			inWord = true
			for i++; i < len(cmd) && cmd[i] != '"'; i++ {
				if cmd[i] == '\\' && i+1 < len(cmd) {
					i++
				}
				word.WriteByte(cmd[i])
			}
			if i >= len(cmd) {
				sure = false
			}
		case ' ', '\t':
			endWord()
		case '#':
			if inWord {
				word.WriteByte(c)
				break
			}
			for i < len(cmd) && cmd[i] != '\n' {
				i++
			}
			i--
		case '(':
			if strings.HasPrefix(strings.TrimLeft(cmd[i+1:], " \t"), ")") {
				sure = false // name(), a function, or a zsh empty list
			}
			endStatement()
		case '\n', ';', '&', '|', ')', '{', '}':
			endStatement()
		default:
			inWord = true
			word.WriteByte(c)
		}
	}
	endStatement()
	return out, sure
}
