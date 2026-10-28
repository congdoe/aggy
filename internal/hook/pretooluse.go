package hook

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
)

// MaxPayload caps a hook payload read from stdin.
const MaxPayload = 1 << 20

// Decision is the guard's answer to one tool call.
type Decision struct {
	Deny   bool
	Reason string
}

// PreToolUse decides on one PreToolUse payload: it denies a tool call
// whose command (tool_input.command or tool_input.cmd, as a string or a
// list of strings) calls claude -p, codex exec, grok -p, or aggy borrow
// (or aggy image or aggy video, which borrow). A payload that is not a
// JSON object is denied: the guard fails closed.
//
// It is an accident guard for delegated children, not a security
// boundary. It matches words, so a shell that builds the command at run
// time ($(which claude) -p, an alias, a script file, base64) gets past
// it, and a command that only mentions "claude -p" in an echo is denied.
// The payload is parsed and matched as text; it never reaches a shell.
func PreToolUse(payload []byte) Decision {
	var p struct {
		ToolName  string         `json:"tool_name"`
		ToolInput map[string]any `json:"tool_input"`
	}
	var probe map[string]any
	if json.Unmarshal(payload, &probe) != nil || probe == nil || json.Unmarshal(payload, &p) != nil {
		return Decision{Deny: true, Reason: "aggy guard: the PreToolUse payload is not a JSON object, so the call is denied"}
	}
	for _, key := range []string{"command", "cmd"} {
		for _, cmd := range strs(p.ToolInput[key]) {
			if what := Nested(cmd); what != "" {
				return Decision{Deny: true, Reason: "aggy guard: a delegated borrow may not run " + what +
					": borrows are one hop. Do the task yourself and report back."}
			}
		}
	}
	return Decision{}
}

func strs(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// separators end a simple command; they become their own token.
const separators = ";&|()<>`$\n"

// words splits a command line into words and separator marks (""),
// dropping quotes, so a command inside sh -c "..." is seen too.
func words(cmd string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range cmd {
		switch {
		case r == '\'' || r == '"' || r == '\\':
			// Quotes and escapes are dropped: their contents are words.
		case strings.ContainsRune(separators, r):
			flush()
			out = append(out, "")
		case r == ' ' || r == '\t' || r == '\r':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// wrapper names a program that runs another one, and the flags it takes
// that have a value. The words after it (its flags, then any VAR=value
// for env) lead to the command it runs.
var wrappers = map[string][]string{
	"env": {"-u", "--unset", "-C", "--chdir", "-S", "--split-string"}, "exec": {"-a"}, "command": nil,
	"builtin": nil, "nohup": nil, "time": {"-f", "-o"}, "nice": {"-n", "--adjustment"},
	"timeout": {"-s", "--signal", "-k", "--kill-after"}, "stdbuf": {"-i", "-o", "-e"},
	"sudo": {"-u", "-g", "-h", "-p", "-C", "-D", "-r", "-t", "-U"}, "doas": {"-u", "-C"},
	"xargs": {"-I", "-n", "-L", "-P", "-d", "-E", "-s", "-a"},
	"npx":   {"-p", "--package", "-c", "--call"}, "bunx": {"-p", "--package"}, "pnpx": nil,
}

// shells run their -c argument as a command.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true}

// codexValueFlags take a value, so they are skipped when finding the
// codex subcommand.
var codexValueFlags = []string{"-c", "--config", "-m", "--model", "-C", "--cd", "-p", "--profile",
	"-s", "--sandbox", "-a", "--ask-for-approval", "-i", "--image", "--enable", "--disable",
	"--add-dir", "--local-provider"}

// program returns the program a word names: its base name, without an
// npm version (@anthropic-ai/claude-code@latest is claude-code), and
// claude-code as claude.
func program(w string) string {
	w = filepath.Base(w)
	if i := strings.LastIndexByte(w, '@'); i > 0 {
		w = w[:i]
	}
	if w == "claude-code" {
		return "claude"
	}
	return w
}

// assignment reports whether w is VAR=value.
func assignment(w string) bool {
	name, _, ok := strings.Cut(w, "=")
	return ok && name != "" && strings.Trim(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_") == ""
}

// skipFlags returns the index of the first word at or after i that is
// not a flag of a program whose value flags are vals.
func skipFlags(ws []string, i int, vals []string) int {
	for i < len(ws) && strings.HasPrefix(ws[i], "-") && ws[i] != "-" {
		name, _, hasVal := strings.Cut(ws[i], "=")
		i++
		if !hasVal && slices.Contains(vals, name) {
			i++
		}
	}
	return i
}

// Nested returns what cmd would start that a delegated child may not
// ("claude -p", "codex exec", "grok -p", "aggy borrow"), or "". Only a
// word at command position counts: the first word of a simple command,
// after VAR=value words, after a wrapper such as env, sudo, timeout,
// xargs, or npx and its flags, after npm, pnpm, or yarn exec/dlx/x, or
// the first word of a shell's -c argument. So git log -S claude -p and
// find . -name claude -print do not match.
//
// TODO(T6737): a CLI after then, do, {, or !, or started by find -exec,
// watch, or ssh, is not at a command position Nested knows (review
// finding R-4).
func Nested(cmd string) string {
	cmd = strings.NewReplacer("${IFS}", " ", "$IFS", " ").Replace(cmd)
	ws := words(cmd)
	for start := 0; start < len(ws); start++ {
		end := start
		for end < len(ws) && ws[end] != "" {
			end++
		}
		if what := nestedIn(ws[start:end]); what != "" {
			return what
		}
		start = end
	}
	return ""
}

// nestedIn checks one simple command's words.
func nestedIn(ws []string) string {
	i := 0
	for i < len(ws) {
		for i < len(ws) && assignment(ws[i]) {
			i++
		}
		if i >= len(ws) {
			return ""
		}
		prog, rest := program(ws[i]), ws[i+1:]
		if vals, ok := wrappers[prog]; ok {
			i = skipFlags(ws, i+1, vals)
			if prog == "timeout" && i < len(ws) {
				i++ // the duration
			}
			continue
		}
		if (prog == "npm" || prog == "pnpm" || prog == "yarn") && len(rest) > 0 &&
			slices.Contains([]string{"exec", "dlx", "x"}, rest[0]) {
			i = skipFlags(ws, i+2, []string{"-p", "--package"})
			continue
		}
		if shells[prog] {
			j := i + 1
			c := false
			for j < len(ws) && strings.HasPrefix(ws[j], "-") {
				if !strings.HasPrefix(ws[j], "--") && strings.ContainsRune(ws[j], 'c') {
					c = true
				}
				j++
			}
			if !c {
				return ""
			}
			i = j
			continue
		}
		switch prog {
		case "claude":
			if hasFlag(rest, "-p", "--print") {
				return "claude -p"
			}
		case "grok":
			if hasFlag(rest, "-p", "--print", "--prompt", "--prompt-file") {
				return "grok -p"
			}
		case "codex":
			if k := skipFlags(rest, 0, codexValueFlags); k < len(rest) && (rest[k] == "exec" || rest[k] == "e") {
				return "codex exec"
			}
		case "aggy":
			if k := skipFlags(rest, 0, nil); k < len(rest) && slices.Contains([]string{"borrow", "image", "video"}, rest[k]) {
				return "aggy " + rest[k]
			}
		}
		return ""
	}
	return ""
}

// hasFlag reports whether args hold one of flags, alone or as
// --flag=value, or a short flag cluster holding a one-letter one (-pv).
func hasFlag(args []string, flags ...string) bool {
	for _, a := range args {
		name, _, _ := strings.Cut(a, "=")
		for _, f := range flags {
			if name == f {
				return true
			}
			if len(f) == 2 && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsRune(a[1:], rune(f[1])) {
				return true
			}
		}
	}
	return false
}
