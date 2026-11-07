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
}

// OpenCodeEnv is what the guard reads from outside the payload.
type OpenCodeEnv struct {
	// Real resolves an existing path, following links. Nil means
	// safefs.RealPath, which also refuses a vendor credential file.
	Real func(string) (string, error)
	// Lstat tells a missing path. Nil means safefs.Lstat.
	Lstat func(string) (fs.FileInfo, error)
	// Getenv finds OpenCode's data folder for its own saved tool output.
	Getenv func(string) string
}

// The refusals' words. They go to the model, as the tool's error.
const (
	whyBadPayload  = "aggy's guard could not read this call, so it was refused."
	whyDeclaration = "aggy's guard refuses a variable declaration in a shell command (OpenCode's permission check skips them, so one before an allowed command could change what it runs). Pass the value on the command it is for, or use a script the person can read."
	whyAuthExport  = "aggy's guard refuses `opencode auth export`: it prints sign-in secrets."
	whyFolder      = "aggy's guard refuses a tool call from another folder than the one this session was launched in: that project's settings and trust were not checked."
	whyOutside     = "aggy's guard refuses a path outside this repo: its real location is elsewhere, or a link points out of the repo. Ask the person to copy it in, if it is needed."
)

// pathTools take input.path, resolved against the session's folder
// (core/src/tool/plugin at v2.2.5). grep and glob default to the folder.
var pathTools = []string{"read", "edit", "write", "grep", "glob"}

// authExportRE matches the command by its words, in any spacing. It sees
// the raw command, so a variable or a script gets past it (docs/design.md).
var authExportRE = regexp.MustCompile(`\bopencode\s+(?:[^\n;&|]*\s)?auth\s+export\b`)

// patchFileRE finds the files a patch tool's text names
// (core/src/patch at v2.2.5: *** Add File:, *** Update File:, *** Delete
// File:, *** Move to:).
var patchFileRE = regexp.MustCompile(`(?m)^\*\*\* (?:Add File|Update File|Delete File|Move to): (.+)$`)

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
	switch {
	case in.Tool == "shell":
		switch {
		case authExportRE.MatchString(in.Command):
			return Decision{Deny: true, Reason: whyAuthExport}
		case hasDeclaration(in.Command):
			return Decision{Deny: true, Reason: whyDeclaration}
		case in.Workdir != "" && !inside(in.Workdir):
			return Decision{Deny: true, Reason: whyOutside}
		}
	case in.Tool == "patch" || in.Tool == "apply_patch":
		for _, m := range patchFileRE.FindAllStringSubmatch(in.PatchText, -1) {
			if !inside(strings.TrimSpace(m[1])) {
				return Decision{Deny: true, Reason: whyOutside}
			}
		}
	case slices.Contains(pathTools, in.Tool) && in.Path != "":
		if !inside(in.Path) {
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
var assignmentRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\+?=`)

// statementWords are shell keywords a statement may start after.
var statementWords = []string{"if", "then", "elif", "else", "while", "until", "do", "!", "time"}

// hasDeclaration reports a statement that only declares: `PATH=x:$PATH;
// git status` ran x's git with no prompt under an allowed `git status`
// (fake model). `FOO=1 git status` does not count: it is one command, which
// the permission check sees whole, and it prompts.
func hasDeclaration(cmd string) bool {
	for _, words := range statements(cmd) {
		i := 0
		for i < len(words) && slices.Contains(statementWords, words[i]) {
			i++
		}
		if i == len(words) {
			continue
		}
		if slices.Contains(declarations, words[i]) {
			return true
		}
		j := i
		for j < len(words) && assignmentRE.MatchString(words[j]) {
			j++
		}
		if j > i && j == len(words) {
			return true
		}
	}
	return false
}

// statements splits a shell command into the words of each statement,
// honouring quotes, comments, and heredoc bodies. It is not a parser: it
// only has to find where statements begin. Quoted text keeps its place in
// a word with its quotes gone, so `"X=1"` is not told from X=1: that errs
// towards refusing.
func statements(cmd string) [][]string {
	var (
		out      [][]string
		words    []string
		word     strings.Builder
		inWord   bool
		heredocs []string
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
		case '<':
			if rest := cmd[i:]; strings.HasPrefix(rest, "<<") && !strings.HasPrefix(rest, "<<<") {
				endWord()
				j := i + 2
				if j < len(cmd) && cmd[j] == '-' {
					j++
				}
				for j < len(cmd) && (cmd[j] == ' ' || cmd[j] == '\t') {
					j++
				}
				k := j
				for k < len(cmd) && !strings.ContainsRune(" \t\n;&|()<>", rune(cmd[k])) {
					k++
				}
				heredocs = append(heredocs, strings.Trim(cmd[j:k], `'"\`))
				words = append(words, "<<"+cmd[j:k])
				i = k - 1
				break
			}
			inWord = true
			word.WriteByte(c)
		case '\n':
			endStatement()
			// A heredoc's body starts at the next line, and holds no statements.
			for _, end := range heredocs {
				for i+1 < len(cmd) {
					nl := strings.IndexByte(cmd[i+1:], '\n')
					line := cmd[i+1:]
					if nl >= 0 {
						line = cmd[i+1 : i+1+nl]
					}
					i += len(line) + 1
					if strings.TrimLeft(line, "\t") == end {
						break
					}
				}
			}
			heredocs = nil
		case ';', '&', '|', '(', ')', '{', '}', '`':
			endStatement()
		case '$':
			if i+1 < len(cmd) && cmd[i+1] == '(' {
				endStatement()
				i++
				break
			}
			inWord = true
			word.WriteByte(c)
		default:
			inWord = true
			word.WriteByte(c)
		}
	}
	endStatement()
	return out
}
