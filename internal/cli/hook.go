package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/congdoe/aggy/internal/hook"
)

const hookUsage = `Usage: aggy hook <event>

Called by a vendor CLI's hook, with the JSON payload on stdin. Events:

  pretooluse   the guard in borrow children: denies shell commands that
               run claude -p, codex exec, grok -p, or aggy borrow. An
               accident guard, not a security boundary.
`

func runHook(args []string, stdout, stderr io.Writer) int {
	return hookWith(args, os.Stdin, stdout, stderr)
}

// hookWith is aggy hook reading the payload from in. The payload is
// parsed as JSON and never reaches a shell. A denial exits 2 with the
// reason on stderr, which Claude Code shows the model and which blocks
// the tool call.
func hookWith(args []string, in io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprint(stderr, hookUsage)
		return 1
	}
	switch args[0] {
	case "-h", "--help":
		fmt.Fprint(stdout, hookUsage)
		return 0
	case "pretooluse", "PreToolUse":
		b, err := io.ReadAll(io.LimitReader(in, hook.MaxPayload+1))
		if err != nil || len(b) > hook.MaxPayload {
			fmt.Fprintln(stderr, "aggy guard: the PreToolUse payload could not be read, so the call is denied")
			return 2
		}
		d := hook.PreToolUse(b)
		if d.Deny {
			fmt.Fprintln(stderr, d.Reason)
			return 2
		}
		return 0
	}
	// Other events are forwarded to the supervisor from M2.
	fmt.Fprintf(stderr, "aggy hook: event %q is not handled yet\n", args[0])
	return 1
}
