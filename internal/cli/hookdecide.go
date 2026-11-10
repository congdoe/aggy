package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/policyhook"
	"github.com/congdoe/aggy/internal/safefs"
)

// aggy hook decide and decide-failed (T6976): the policy decision hook a
// launch wires into Claude's PreToolUse and OpenCode's permission evaluate,
// and the record its wrapper makes when it fails. Every call writes a start
// and an end record to the diagnostic log, through the queue that never waits
// on the disk, with the vendor, the CLI's version from the launch's plan, the
// decision, the policy group, a reason code, and the duration: never the
// command, the path, or a reason's text. A start with no end is a hang or a
// kill. A failed write never changes the decision.

// decideLogWait bounds how long the hook waits, after it has answered, for
// its records to be written.
const decideLogWait = time.Second

// decideArgs reads --vendor claude|opencode --plan <absolute path> --sha256
// <hex>: the plan and the hash the launch wrote it with, both from the hook's
// own command, which the session cannot change (PR #129 review, HIGH-1).
func decideArgs(args []string) (v adapter.Vendor, plan, sum string, ok bool) {
	if len(args) != 6 || args[0] != "--vendor" || args[2] != "--plan" || args[4] != "--sha256" {
		return "", "", "", false
	}
	v, err := adapter.ParseVendor(args[1])
	if err != nil || (v != adapter.Claude && v != adapter.OpenCode) {
		return "", "", "", false
	}
	if !filepath.IsAbs(args[3]) || filepath.Clean(args[3]) != args[3] || !policyhook.ValidSum(args[5]) {
		return "", "", "", false
	}
	return v, args[3], args[5], true
}

// readDecidePlan reads the launch's plan, which must hash to sum.
func readDecidePlan(path, sum string) (policyhook.DecidePlan, error) {
	b, err := safefs.ReadFileLimit(path, policyhook.MaxDecidePlan)
	if err != nil {
		return policyhook.DecidePlan{}, err
	}
	return policyhook.CheckPlan(b, sum)
}

// decideCodes are the end record's codes, by the decision's reason.
var decideCodes = map[parity.Reason]diag.Code{
	parity.ReasonNoMatch:      diag.CodeDecideNoMatch,
	parity.ReasonPreApproved:  diag.CodeDecidePreApproved,
	parity.ReasonPolicyMatch:  diag.CodeDecideAsked,
	parity.ReasonGroupDenied:  diag.CodeDecideGroupDenied,
	parity.ReasonNoPrompt:     diag.CodeDecideNoPrompt,
	parity.ReasonUnknownInput: diag.CodeDecideUnreadable,
	parity.ReasonTooLong:      diag.CodeDecideTooLong,
	parity.ReasonProtected:    diag.CodeDecideProtected,
	parity.ReasonOtherTool:    diag.CodeDecideOtherTool,
	parity.ReasonPlanChanged:  diag.CodeDecidePlanChanged,
}

// hookDecide is aggy hook decide. On Claude it prints a PreToolUse decision,
// or nothing for no opinion; on OpenCode one word. A plan that is missing,
// unreadable, or does not hash to the --sha256 the launch put in the command
// (PR #129 review, HIGH-1) is never used: the call is asked about, or refused
// where the payload's mode shows no prompt, and the end record carries a
// failure code. It exits 1 only on bad arguments, so Claude's wrapper falls
// back to asking and OpenCode's plugin to asking for an allow.
func hookDecide(args []string, in io.Reader, stdout, stderr io.Writer) int {
	start := time.Now()
	v, planPath, sum, ok := decideArgs(args)
	if !ok {
		drain(in)
		return cmdUsage(stderr, "hook decide", "want --vendor claude|opencode --plan <absolute path> --sha256 <hex>", hookUsage)
	}
	q := dlog.Queue(8)
	defer q.Close(decideLogWait)
	plan, planErr := readDecidePlan(planPath, sum)
	vv := plan.VendorVersion
	if vv == "" {
		vv = "unknown"
	}
	startID := q.Log(diag.Rec{Event: diag.EvDecideStarted, Vendor: string(v), Attrs: map[string]any{"vendor_version": vv}})
	end := func(code diag.Code, decision, group string) {
		q.Log(diag.Rec{Event: diag.EvDecideEnded, Code: code, Vendor: string(v), Cause: startID, Duration: max(time.Since(start), time.Millisecond),
			Attrs: map[string]any{"vendor_version": vv, "decision": decision, "group": group}})
	}
	raw, err := io.ReadAll(io.LimitReader(in, hook.MaxPayload+1))
	if err != nil || len(raw) > hook.MaxPayload {
		raw = nil // judged as unreadable: asked, never allowed
	}
	var d parity.CallDecision
	var out []byte
	switch {
	case planErr != nil:
		code := diag.CodeDecidePlan
		if errors.Is(planErr, policyhook.ErrPlanChanged) {
			code = diag.CodeDecidePlanChanged
		}
		// Only a call the hook would judge asks (PR #129 re-review).
		if v == adapter.OpenCode {
			calls, ok := policyhook.OpenCodeCalls(raw, "")
			d = policyhook.PlanFailed(!ok || len(calls) > 0, "")
			out = policyhook.OpenCodeOutput(d)
		} else {
			calls, mode, ok := policyhook.ClaudeCalls(raw)
			d = policyhook.PlanFailed(!ok || len(calls) > 0, mode)
			out = policyhook.ClaudeOutput(d)
		}
		_, _ = stdout.Write(out)
		end(code, string(d.Verdict), "")
		fmt.Fprintf(stderr, "aggy hook decide: the launch's decision plan is missing, unreadable, or changed, so this call is asked about %s\n", dlog.Tag(code))
		return 0
	case v == adapter.OpenCode:
		calls, ok := policyhook.OpenCodeCalls(raw, plan.Root)
		d = policyhook.JudgeOpenCode(plan, calls, ok)
		out = policyhook.OpenCodeOutput(d)
	default:
		calls, mode, ok := policyhook.ClaudeCalls(raw)
		d = policyhook.Judge(plan, calls, mode, ok)
		out = policyhook.ClaudeOutput(d)
	}
	_, _ = stdout.Write(out)
	end(decideCodes[d.Reason], string(d.Verdict), d.Group)
	return 0
}

// hookDecideFailed is aggy hook decide-failed: the wrapper's one attempt to
// record that aggy hook decide failed and the fixed fallback answered. It reads
// nothing but its plan's vendor version, and its output goes nowhere.
func hookDecideFailed(args []string, stderr io.Writer) int {
	v, planPath, sum, ok := decideArgs(args)
	if !ok {
		return cmdUsage(stderr, "hook decide-failed", "want --vendor claude|opencode --plan <absolute path> --sha256 <hex>", hookUsage)
	}
	vv := "unknown"
	if plan, err := readDecidePlan(planPath, sum); err == nil && plan.VendorVersion != "" {
		vv = plan.VendorVersion
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()
	dlog.Log(ctx, diag.Rec{Event: diag.EvDecideFallback, Code: diag.CodeDecideFallback, Vendor: string(v),
		Attrs: map[string]any{"vendor_version": vv, "decision": "fallback"}})
	return 0
}
