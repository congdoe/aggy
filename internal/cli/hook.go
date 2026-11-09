package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/termsafe"
)

const hookUsage = `Usage: aggy hook pretooluse
       aggy hook opencode-guard
       aggy hook opencode-always
       aggy hook --vendor V <event>

Called by a vendor CLI's hook, with the JSON payload on stdin. aggy hooks
install writes these calls into each CLI's hook config.

  pretooluse   the guard in borrow children: denies shell commands that
               run claude -p, codex exec, grok -p, or aggy borrow. An
               accident guard, not a security boundary.
  opencode-guard
               the check aggy's OpenCode plugin runs before each tool call:
               refuses, beside an allowed command, one that names a variable
               that command reads or defines a function by its name; a real
               path outside the repo; and opencode auth export. Mistakes only,
               not a security boundary.
  opencode-always
               what the plugin runs when a permission is answered with Always
               allow: from then on the guard scans every command in this repo.
  <event>      a Claude, Codex, or Grok hook event (Stop, StopFailure,
               StopCancelled, SessionStart, Notification, and on Claude
               and Grok UserPromptSubmit, which blocks a command typed
               inside the session before the model sees it). The
               payload is parsed as JSON and forwarded to the aggy
               supervisor running the CLI ($AGGY_SOCKET). Outside aggy,
               or on any failure, it exits 0 and prints nothing to
               stdout, so the vendor CLI is never disturbed; for a command
               aggy takes, UserPromptSubmit prints only its block decision.
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
		case "opencode-guard":
			return openCodeGuard(in, getenv, stderr)
		case "opencode-always":
			return openCodeAlways(in, stderr)
		}
	}
	if len(args) != 3 || args[0] != "--vendor" {
		cmdUsage(stderr, "hook", "bad arguments", hookUsage)
		return 1
	}
	v, err := adapter.ParseVendor(args[1])
	if err != nil {
		cmdUsage(stderr, "hook", err.Error(), "")
		return 1
	}
	event := args[2]
	if event == "UserPromptSubmit" || event == "UserPromptExpansion" && v == adapter.Claude {
		return promptSubmit(v, in, getenv, stdout, stderr)
	}
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
		fmt.Fprintln(stderr, "aggy guard: the PreToolUse payload could not be read, so the call is denied", dlog.Tag(diag.CodeOneHop))
		return 2
	}
	d := hook.PreToolUse(b)
	if d.Deny {
		fmt.Fprintln(stderr, d.Reason, dlog.Tag(diag.CodeOneHop))
		return 2
	}
	return 0
}

// openCodeAlwaysMark is the file that says repo root has had a permission
// answered with Always allow under aggy. OpenCode saves that rule per project
// in opencode.db, which aggy never opens, and it outlives the session, so this
// outlives it too.
func openCodeAlwaysMark(root string) (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(dir, "opencode-always", hex.EncodeToString(sum[:])), nil
}

// openCodeAlwaysSeen reads the mark. Anything but a clean "not there" counts as
// seen, as that only makes the guard scan more.
func openCodeAlwaysSeen(root string) bool {
	mark, err := openCodeAlwaysMark(root)
	if err != nil {
		return true
	}
	_, err = safefs.Lstat(mark)
	return !errors.Is(err, fs.ErrNotExist)
}

// openCodeAlways records the mark for the plugin. The plugin also holds this
// for its session, so a failure here is not the only copy.
func openCodeAlways(in io.Reader, stderr io.Writer) int {
	var p struct {
		Root string `json:"root"`
	}
	b, err := io.ReadAll(io.LimitReader(in, hook.MaxPayload))
	if err == nil {
		err = json.Unmarshal(b, &p)
	}
	var root, mark string
	if err == nil {
		root, err = safefs.RealPath(p.Root)
	}
	if err == nil {
		mark, err = openCodeAlwaysMark(root)
	}
	if err == nil {
		err = safefs.MkdirAll(filepath.Dir(mark), 0o700)
	}
	if err == nil {
		err = safefs.WriteFile(mark, nil, 0o600)
	}
	if err != nil {
		fmt.Fprintf(stderr, "aggy hook opencode-always: %s %s\n", termsafe.Line(err.Error()), dlog.Tag(diag.CodeUsage))
		return 1
	}
	return 0
}

// openCodeGuard runs the check behind the plugin's execute.before. Its exit
// code decides, so a crash or a hang refuses too; the plugin passes stderr
// to the model as the tool's error, ending with the reason code and run id.
func openCodeGuard(in io.Reader, getenv func(string) string, stderr io.Writer) int {
	b, err := io.ReadAll(io.LimitReader(in, hook.MaxPayload+1))
	if err != nil || len(b) > hook.MaxPayload {
		b = nil // refused as unreadable
	}
	extra, scanAlways := guardRules(b, getenv)
	rules, err := parity.OpenCodeShellRules(extra)
	if err != nil {
		b = nil // refused as unreadable: the rules aggy ships do not render
	}
	d := hook.OpenCodeGuard(b, hook.OpenCodeEnv{Getenv: getenv, DeniedNames: parity.OpenCodeDeniedNames(), ShellRules: rules, ScanAlways: scanAlways, AlwaysSeen: openCodeAlwaysSeen})
	if !d.Deny {
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()
	dlog.Log(ctx, diag.Rec{Event: diag.EvGuardRefused, Code: diag.CodeGuardRefused, Msg: "aggy's guard refused an OpenCode tool call", Vendor: string(adapter.OpenCode)})
	fmt.Fprintf(stderr, "%s %s\n", d.Reason, dlog.Tag(diag.CodeGuardRefused))
	return 2
}
