package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/supervisor"
)

const hookUsage = `Usage: aggy hook pretooluse
       aggy hook --vendor V <event>

Called by a vendor CLI's hook, with the JSON payload on stdin. aggy hooks
install writes these calls into each CLI's hook config.

  pretooluse   the guard in borrow children: denies shell commands that
               run claude -p, codex exec, grok -p, or aggy borrow. An
               accident guard, not a security boundary.
  <event>      a Claude, Codex, or Grok hook event (Stop, StopFailure,
               StopCancelled, SessionStart, Notification). The
               payload is parsed as JSON and forwarded to the aggy
               supervisor running the CLI ($AGGY_SOCKET). Outside aggy,
               or on any failure, it exits 0 and prints nothing to
               stdout, so the vendor CLI is never disturbed.
`

// hookTimeout bounds a forwarded hook from start to reply. The vendor
// waits for its hook; aggy must never hold up a turn.
const hookTimeout = 3 * time.Second

func runHook(args []string, stdout, stderr io.Writer) int {
	return hookWith(args, os.Stdin, os.Getenv, stdout, stderr)
}

// hookWith is aggy hook reading the payload from in. The payload is
// parsed as JSON and never reaches a shell. A PreToolUse denial exits 2
// with the reason on stderr, which Claude Code shows the model and which
// blocks the tool call.
//
// A forwarded event never writes to stdout: Claude adds a SessionStart
// hook's stdout to the model's context (hooks.md, "exit code 0").
func hookWith(args []string, in io.Reader, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 1 {
		switch args[0] {
		case "-h", "--help":
			fmt.Fprint(stdout, hookUsage)
			return 0
		case "pretooluse", "PreToolUse":
			return preToolUse(in, stderr)
		}
	}
	if len(args) != 3 || args[0] != "--vendor" {
		fmt.Fprint(stderr, hookUsage)
		return 1
	}
	v, err := adapter.ParseVendor(args[1])
	if err != nil {
		fmt.Fprintf(stderr, "aggy hook: %v\n", err)
		return 1
	}
	event := args[2]
	if !hook.Forward(v, getenv) {
		// Not under aggy, or another vendor's hook (see hook.Forward).
		// Read the payload anyway, so the CLI's write to the hook's
		// stdin never fails on a closed pipe.
		drain(in)
		return 0
	}
	done := make(chan string, 1)
	go func() { done <- forward(v, event, in, getenv) }()
	select {
	case msg := <-done:
		if msg != "" {
			fmt.Fprintf(stderr, "aggy hook: %s\n", msg)
		}
	case <-time.After(hookTimeout):
		fmt.Fprintln(stderr, "aggy hook: the supervisor did not answer in time; ignored")
	}
	return 0
}

// forward reads the payload, keeps the fields aggy uses, and sends it to
// the supervisor. It returns why it did not, or "".
func forward(v adapter.Vendor, event string, in io.Reader, getenv func(string) string) string {
	raw, err := io.ReadAll(io.LimitReader(in, hook.MaxPayload+1))
	if err != nil || len(raw) > hook.MaxPayload {
		return "the payload could not be read; ignored"
	}
	payload, err := hook.Project(v, raw)
	if err != nil {
		return err.Error() + "; ignored"
	}
	dir, err := home.Dir()
	if err != nil {
		return err.Error()
	}
	resp, err := supervisor.Send(filepath.Join(dir, "run"), getenv("AGGY_SOCKET"),
		supervisor.Request{Type: supervisor.ReqHookEvent, Vendor: string(v), Event: event, Payload: payload}, hookTimeout)
	switch {
	case err != nil:
		return err.Error()
	case !resp.OK:
		return "the supervisor refused the event: " + resp.Error
	}
	return ""
}

// drain reads and drops in, for at most hookTimeout and MaxPayload bytes.
func drain(in io.Reader) {
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(in, hook.MaxPayload))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(hookTimeout):
	}
}

// preToolUse runs the borrow children's guard.
func preToolUse(in io.Reader, stderr io.Writer) int {
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
