package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/sessioncmd"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/termsafe"
)

// promptSubmit is aggy hook for UserPromptSubmit, on Claude and Grok (T6910):
// the second catch of the commands inside a session, for a line picked from
// the CLI's own menu, whose keystrokes do not spell the command. Typed in
// full, the supervisor already caught it from the keys and no hook runs.
//
// For a command that starts no model turn, under aggy, it hands the line to
// the supervisor and, when the supervisor takes it, blocks the prompt with a
// JSON decision on stdout: Claude's hooks.md, UserPromptSubmit, "decision:
// block stops the prompt before it reaches Claude", reason "Shown to the user
// ... Not added to context", suppressOriginalPrompt inside hookSpecificOutput leaves the text out of that
// message (read 2026-11-09, not seen live); Grok's 10-hooks.md at grok-build
// 2bdd1d6a: {"decision": "block", "reason": "..."} on stdout blocks on any exit
// code, the reason is shown to the user and not added to the model's context
// (read 2026-11-09, not seen live). JSON, not exit 2: aggy's hook command ends
// in || true, which turns every exit into 0 (hook.Command).
//
// Anything else prints nothing and exits 0, so the prompt goes on: a line that
// is no command, one with a model turn (/borrow with a task, /aggy with a
// question), a keyboard-only one, and any command outside aggy, where the
// skill tells the model to say to start the session with aggy. On Grok a hook
// that times out lets the prompt through too (10-hooks.md: "a timed-out or
// crashed hook fails open"). Nothing of the prompt is forwarded but a parsed
// command's own line.
func promptSubmit(v adapter.Vendor, event string, in io.Reader, getenv func(string) string, stdout, stderr io.Writer) int {
	if !hook.Forward(v, getenv) {
		drain(in)
		return 0
	}
	raw, err := io.ReadAll(io.LimitReader(in, hook.MaxPayload+1))
	if err != nil || len(raw) > hook.MaxPayload {
		return 0
	}
	// Claude names the text prompt (hooks.md, UserPromptSubmit input). Grok's
	// field for it is not in 10-hooks.md: prompt is unverified there, and a
	// payload without it takes nothing.
	var p struct {
		Prompt string `json:"prompt"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return 0
	}
	line := strings.TrimRight(p.Prompt, " \r\n")
	c, ok := sessioncmd.Parse(v, line)
	if !ok || c.KeyboardOnly {
		return 0
	}
	dir, err := home.Dir()
	if err != nil {
		return 0
	}
	resp, err := supervisor.Send(filepath.Join(dir, "run"), getenv("AGGY_SOCKET"),
		supervisor.Request{Type: supervisor.ReqPromptCommand, Vendor: string(v), Command: line}, hookTimeout)
	if err != nil || !resp.OK {
		why := "the supervisor's socket could not be reached"
		code := diag.CodeUnreachable
		if err == nil {
			why = "the supervisor did not take the command: " + resp.Error
			code = diag.CodeCommandNotTaken
			if c, ok := diag.CodeNamed(resp.Code); ok {
				code = c.Name
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		tag := ""
		if id := dlog.Log(ctx, diag.Rec{Event: diag.EvHookForwardFailed, Code: code, Msg: "a command from the prompt hook went to the CLI", Vendor: string(v), NoRefusal: err == nil}); id != "" {
			tag = " " + dlog.Tag(code)
		}
		fmt.Fprintf(stderr, "aggy hook: %s%s\n", termsafe.Line(why), tag)
		return 0
	}
	reason := fmt.Sprintf("aggy took %s%s: see aggy's screen (Esc returns). It spent no model turn.", sessioncmd.Prefix(v), c.Name)
	if resp.Type == supervisor.RespAsked {
		// The supervisor took nothing yet: it asks on its status line (T6982).
		reason = fmt.Sprintf("aggy asked about %s%s on its status line; it spent no model turn.", sessioncmd.Prefix(v), c.Name)
	}
	out := map[string]any{"decision": "block", "reason": reason}
	if v == adapter.Claude {
		// Inside hookSpecificOutput, as hooks.md's UserPromptSubmit example
		// has it (read 2026-11-11); at the top level Claude ignored it and the
		// block message ended "Original prompt: /switch" (owner's run, T6982).
		out["hookSpecificOutput"] = map[string]any{"hookEventName": event, "suppressOriginalPrompt": true}
	}
	b, _ := json.Marshal(out)
	fmt.Fprintln(stdout, string(b))
	return 0
}
