package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/termsafe"
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
	deadline := time.Now().Add(hookTimeout)
	// The record joins the session's trace: aggy hook runs with the
	// AGGY_TRACE of the supervisor it reports to. It holds aggy's own
	// words only, never the payload or what the socket said. The vendor
	// waits for its hook, so the record gets only the time left before
	// the deadline (G67-6); one not written in time is not tagged.
	failed := func(f hookFailure) {
		written := make(chan string, 1)
		go func() {
			written <- dlog.Log(context.Background(), diag.Rec{Event: diag.EvHookForwardFailed, Code: f.code, Msg: f.why, Vendor: string(v), NoRefusal: f.relayed})
		}()
		tag := ""
		select {
		case id := <-written:
			if id != "" {
				tag = " " + dlog.Tag(f.code)
			}
		case <-time.After(time.Until(deadline)):
		}
		fmt.Fprintf(stderr, "aggy hook: %s%s\n", termsafe.Line(f.shown), tag)
	}
	done := make(chan *hookFailure, 1)
	go func() { done <- forward(v, event, in, getenv) }()
	select {
	case f := <-done:
		if f != nil {
			failed(*f)
		}
	case <-time.After(time.Until(deadline)):
		// No time is left for a record.
		fmt.Fprintln(stderr, "aggy hook: the supervisor did not answer in time; ignored")
	}
	return 0
}

// hookFailure is why aggy hook did not forward an event: its code, a
// record's message, and what aggy prints.
type hookFailure struct {
	code       diag.Code
	why, shown string
	// relayed: the code is the supervisor's, which recorded the refusal.
	relayed bool
}

// forward reads the payload, keeps the fields aggy uses, and sends it to
// the supervisor. It returns why it did not, or nil.
func forward(v adapter.Vendor, event string, in io.Reader, getenv func(string) string) *hookFailure {
	raw, err := io.ReadAll(io.LimitReader(in, hook.MaxPayload+1))
	if err != nil || len(raw) > hook.MaxPayload {
		const msg = "the payload could not be read; ignored"
		return &hookFailure{diag.CodeBadPayload, msg, msg, false}
	}
	payload, err := hook.Project(v, raw)
	if err != nil {
		return &hookFailure{diag.CodeBadPayload, "the payload is not a JSON object; ignored", err.Error() + "; ignored", false}
	}
	dir, err := home.Dir()
	if err != nil {
		return &hookFailure{diag.CodeStore, "the state directory could not be found", err.Error(), false}
	}
	resp, err := supervisor.Send(filepath.Join(dir, "run"), getenv("AGGY_SOCKET"),
		supervisor.Request{Type: supervisor.ReqHookEvent, Vendor: string(v), Event: event, Payload: payload}, hookTimeout)
	switch {
	case err != nil:
		return &hookFailure{diag.CodeUnreachable, "the supervisor's socket could not be reached", err.Error(), false}
	case !resp.OK:
		code := diag.CodeBadRequest
		if c, ok := diag.CodeNamed(resp.Code); ok {
			code = c.Name
		}
		return &hookFailure{code, "the supervisor refused the event", "the supervisor refused the event: " + resp.Error, true}
	}
	return nil
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
