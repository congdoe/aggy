package hook

import (
	"encoding/json"
	"path/filepath"
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
	if err := json.Unmarshal(payload, &p); err != nil {
		return Decision{Deny: true, Reason: "aggy guard: the PreToolUse payload is not JSON, so the call is denied"}
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

// Nested returns what cmd would start that a delegated child may not
// ("claude -p", "codex exec", "grok -p", "aggy borrow"), or "".
func Nested(cmd string) string {
	ws := words(cmd)
	for i, w := range ws {
		prog := filepath.Base(w)
		var rest []string
		for _, x := range ws[i+1:] {
			if x == "" {
				break
			}
			rest = append(rest, x)
		}
		switch prog {
		case "claude":
			if hasFlag(rest, "-p", "--print") {
				return "claude -p"
			}
		case "grok":
			if hasFlag(rest, "-p", "--print", "--prompt") {
				return "grok -p"
			}
		case "codex":
			if sub := firstArg(rest); sub == "exec" || sub == "e" {
				return "codex exec"
			}
		case "aggy":
			if sub := firstArg(rest); sub == "borrow" || sub == "image" || sub == "video" {
				return "aggy " + sub
			}
		}
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

// firstArg returns the first argument that is not a flag.
func firstArg(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}
