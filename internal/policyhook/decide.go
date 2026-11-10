// Package policyhook is aggy's policy decision hook (T6976, docs/design.md,
// section 10): aggy hook decide reads one tool call from a vendor's hook and
// answers from the launch's DecidePlan, a file aggy wrote for that launch,
// through parity's one decision (PolicyPlan.DecideCall). Nothing in the
// payload but the tool input decides; the session's own permission mode may
// only turn an ask into a deny. It also builds the hook's command for Claude,
// with its fixed fallback. It reads no file and runs nothing itself.
package policyhook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
)

// DecidePlanVersion is the plan format this aggy reads.
const DecidePlanVersion = 1

// MaxDecidePlan caps the plan file.
const MaxDecidePlan = 1 << 20

// DecidePlan is what a launch tells its decision hook, in a file in the
// launch's private folder that only the hook command's argv names.
type DecidePlan struct {
	Version int    `json:"version"`
	Vendor  string `json:"vendor"`
	// VendorVersion is the CLI's version from the probe cache, or "unknown",
	// for the log.
	VendorVersion string `json:"vendor_version"`
	// NoPrompt is a session that cannot show a prompt (bypassPermissions or
	// dontAsk however set at launch): every ask is a deny.
	NoPrompt bool `json:"no_prompt"`
	// Settings are the person's group settings, ask or deny.
	Settings map[string]string `json:"settings,omitempty"`
	// Branches are default branches besides parity.ProtectedBranches.
	Branches []string `json:"branches,omitempty"`
	// Commands and Reads are the exact pre-approvals (parity.PreApprovedFrom).
	Commands []string `json:"commands,omitempty"`
	Reads    []string `json:"reads,omitempty"`
}

// NewDecidePlan builds the plan of an on-screen launch.
func NewDecidePlan(vendor, vendorVersion string, noPrompt bool, plan parity.PolicyPlan, pre parity.PreApproved) DecidePlan {
	if vendorVersion == "" {
		vendorVersion = "unknown"
	}
	d := DecidePlan{Version: DecidePlanVersion, Vendor: vendor, VendorVersion: vendorVersion, NoPrompt: noPrompt,
		Branches: plan.Branches, Commands: pre.Commands, Reads: pre.Reads}
	for _, g := range permissions.PolicyGroups {
		if k, ok := plan.Settings[g]; ok {
			if d.Settings == nil {
				d.Settings = map[string]string{}
			}
			d.Settings[g] = string(k)
		}
	}
	return d
}

// ParseDecidePlan reads a plan: one JSON object of this version, no unknown
// key, and only ask or deny for a known group. Anything else is an error, and
// the hook's caller falls back to asking.
func ParseDecidePlan(b []byte) (DecidePlan, error) {
	var d DecidePlan
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return DecidePlan{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return DecidePlan{}, errors.New("hook: text after the decision plan")
	}
	if d.Version != DecidePlanVersion {
		return DecidePlan{}, fmt.Errorf("hook: decision plan version %d, want %d", d.Version, DecidePlanVersion)
	}
	for g, s := range d.Settings {
		if err := permissions.CheckPolicySetting(g, s); err != nil {
			return DecidePlan{}, err
		}
	}
	return d, nil
}

// Policy is the plan's on-screen policy plan.
func (d DecidePlan) Policy() parity.PolicyPlan {
	p := parity.PolicyPlan{OnScreen: true, Branches: d.Branches}
	for g, s := range d.Settings {
		if p.Settings == nil {
			p.Settings = map[string]permissions.Kind{}
		}
		p.Settings[g] = permissions.Kind(s)
	}
	return p
}

// PreApproved is the plan's exact pre-approvals.
func (d DecidePlan) PreApproved() parity.PreApproved {
	return parity.PreApproved{Commands: d.Commands, Reads: d.Reads}
}

// ClaudeHookTools are the Claude Code tools aggy's decision hook is matched
// to: the shell, and every built-in tool a Read rule applies to, which
// permissions.md names as Read, Edit (with Write and the other editing tools)
// and, best effort, Grep and Glob ("Read and Edit", read 2026-11-10). A tool
// not listed never reaches the hook; the matcher is a regular expression of
// these names (hooks.md, "Matchers").
var ClaudeHookTools = []string{"Bash", "Read", "Edit", "Write", "MultiEdit", "NotebookEdit", "Grep", "Glob"}

// ClaudeHookMatcher is the hook entry's matcher.
func ClaudeHookMatcher() string {
	return "^(" + joinNames(ClaudeHookTools) + ")$"
}

func joinNames(ns []string) string {
	out := ""
	for i, n := range ns {
		if i > 0 {
			out += "|"
		}
		out += n
	}
	return out
}

// claudeToolInput is the part of a PreToolUse payload the hook reads
// (hooks.md, "PreToolUse input"): the tool, its input, and the session's
// permission mode, which may only tighten.
type claudeToolInput struct {
	ToolName       string                     `json:"tool_name"`
	ToolInput      map[string]json.RawMessage `json:"tool_input"`
	PermissionMode string                     `json:"permission_mode"`
}

// noPromptModes are Claude's modes in which a prompt is never shown:
// bypassPermissions runs every call, and dontAsk "auto-denies tools unless
// pre-approved" (permission-modes.md).
var noPromptModes = []string{"bypassPermissions", "dontAsk"}

// NoPromptMode says Claude cannot show a prompt in mode.
func NoPromptMode(mode string) bool { return slices.Contains(noPromptModes, mode) }

// ClaudeCalls reads one PreToolUse payload into the calls to judge, and the
// session's permission mode. ok is false for a payload that is not a JSON
// object, names a tool the hook does not know, or lacks the field the tool
// needs: such a call is asked about, never allowed.
func ClaudeCalls(payload []byte) (calls []parity.Call, mode string, ok bool) {
	var in claudeToolInput
	var probe map[string]json.RawMessage
	if json.Unmarshal(payload, &probe) != nil || probe == nil || json.Unmarshal(payload, &in) != nil {
		return nil, "", false
	}
	field := func(k string) (string, bool) {
		raw, has := in.ToolInput[k]
		if !has {
			return "", false
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		return s, true
	}
	one := func(kind parity.CallKind, key string) ([]parity.Call, string, bool) {
		s, has := field(key)
		if !has || s == "" {
			return nil, in.PermissionMode, false
		}
		return []parity.Call{{Kind: kind, Text: s}}, in.PermissionMode, true
	}
	switch in.ToolName {
	case "Bash":
		return one(parity.CallShell, "command")
	case "Read":
		return one(parity.CallRead, "file_path")
	case "Edit", "Write", "MultiEdit":
		return one(parity.CallWrite, "file_path")
	case "NotebookEdit":
		return one(parity.CallWrite, "notebook_path")
	case "Grep", "Glob":
		keys := []string{"path", "glob"}
		if in.ToolName == "Glob" {
			keys = []string{"pattern", "path"}
		}
		for _, k := range keys {
			s, has := field(k)
			if _, present := in.ToolInput[k]; present && !has {
				return nil, in.PermissionMode, false // not a string
			}
			if s != "" {
				calls = append(calls, parity.Call{Kind: parity.CallSearch, Text: s})
			}
		}
		return calls, in.PermissionMode, true
	}
	return nil, in.PermissionMode, false
}

// verdictRank orders verdicts for combining several calls: deny, then ask,
// then allow, then no opinion, as Claude combines hooks (hooks.md).
var verdictRank = []parity.Verdict{parity.VerdictDeny, parity.VerdictAsk, parity.VerdictAllow, parity.VerdictNone}

// Judge decides calls by the plan. A call it could not read (ok false) asks.
// A session that cannot show a prompt (the plan's NoPrompt, or a mode from the
// payload in noPromptModes) gets a deny for every ask. It never allows what it
// could not read.
func Judge(plan DecidePlan, calls []parity.Call, mode string, ok bool) parity.CallDecision {
	d := parity.CallDecision{Verdict: parity.VerdictAsk, Reason: parity.ReasonUnknownInput}
	if ok {
		d = parity.CallDecision{Verdict: parity.VerdictNone, Reason: parity.ReasonNoMatch}
		pol, pre := plan.Policy(), plan.PreApproved()
		for _, c := range calls {
			cd := pol.DecideCall(c, pre)
			if slices.Index(verdictRank, cd.Verdict) < slices.Index(verdictRank, d.Verdict) {
				d = cd
			}
		}
	}
	if d.Verdict == parity.VerdictAsk && (plan.NoPrompt || NoPromptMode(mode)) {
		d.Verdict, d.Reason = parity.VerdictDeny, parity.ReasonNoPrompt
	}
	return d
}

// The reasons Claude shows with a decision. They are fixed text and a group
// name, never anything from the tool input.
const (
	reasonAllow    = "aggy: you pre-approved this exact command with aggy permissions add allow (%s)"
	reasonAsk      = "aggy's safety policy (%s): approve this yourself"
	reasonUnread   = "aggy could not read this call, so it asks: approve this yourself"
	reasonDeny     = "aggy's safety policy refuses this: you set %s to deny (aggy permissions policy)"
	reasonNoPrompt = "aggy's safety policy (%s) asks about this, and this session cannot show a prompt, so it is refused"
)

// ClaudeOutput is the hook's stdout for d: a PreToolUse decision
// (hooks.md, "PreToolUse decision control": hookSpecificOutput with
// permissionDecision allow, ask, or deny, and permissionDecisionReason), or
// nothing for no opinion, which leaves the call to Claude's own flow.
func ClaudeOutput(d parity.CallDecision) []byte {
	var why string
	group := d.Group
	if group == "" {
		group = "an unreadable call"
	}
	switch {
	case d.Verdict == parity.VerdictNone:
		return nil
	case d.Verdict == parity.VerdictAllow:
		why = fmt.Sprintf(reasonAllow, group)
	case d.Reason == parity.ReasonNoPrompt:
		why = fmt.Sprintf(reasonNoPrompt, group)
	case d.Verdict == parity.VerdictDeny:
		why = fmt.Sprintf(reasonDeny, group)
	case d.Reason == parity.ReasonUnknownInput:
		why = reasonUnread
	default:
		why = fmt.Sprintf(reasonAsk, group)
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       string(d.Verdict),
		"permissionDecisionReason": why,
	}})
	return append(b, '\n')
}

// DecideTimeout is the decision hook entry's timeout, in seconds. With
// OnFailure "block", a hook past it blocks the call (live 2026-11-10,
// docs/verification.md).
const DecideTimeout = 10

// The wrapper's fixed outputs. Each is a constant: nothing from the tool
// input, the plan, or the environment goes in, and the apostrophe is a JSON
// escape so the text sits in single quotes in the shell.
const (
	// jsonApos is JSON's escape for an apostrophe: a backslash, then u0027.
	jsonApos     = "\x5cu0027"
	fallbackAsk  = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask","permissionDecisionReason":"aggy` + jsonApos + `s safety check failed; approve this yourself"}}`
	fallbackDeny = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"aggy` + jsonApos + `s safety check failed, and this session cannot show a prompt, so the call is refused"}}`
)

// FallbackOutput is what the wrapper prints when aggy's hook fails.
func FallbackOutput(noPrompt bool) string {
	if noPrompt {
		return fallbackDeny
	}
	return fallbackAsk
}

// DecideCommand is the decision hook entry's command, for sh -c (hooks.md):
//
//	<aggy> hook decide --vendor claude --plan <plan> ||
//	  { <aggy> hook decide-failed ... </dev/null >/dev/null 2>&1 & printf '%s\n' '<fixed decision>'; }
//
// When aggy exits non-zero or is missing, the fallback asks for every call
// (or denies, where no prompt can show), as it cannot judge the policy, and
// makes one attempt to record the failure in the background, its output
// sent nowhere so Claude never waits on it. Both paths must pass CheckBin,
// so neither needs quoting.
func DecideCommand(bin, plan string, noPrompt bool) (string, error) {
	if err := hook.CheckBin(bin); err != nil {
		return "", err
	}
	if err := hook.CheckBin(plan); err != nil {
		return "", fmt.Errorf("the decision plan's path cannot go in a hook command: %w", err)
	}
	args := " --vendor claude --plan " + plan
	return bin + " hook decide" + args + " || { " + bin + " hook decide-failed" + args +
		" </dev/null >/dev/null 2>&1 & printf '%s\\n' '" + FallbackOutput(noPrompt) + "'; }", nil
}

// OpenCodeDecideInput is what aggy's OpenCode plugin sends from a permission
// evaluate event (internal/adapter/opencode/plugin/server.js): the action, its
// resources, and the effect OpenCode's rules gave.
type OpenCodeDecideInput struct {
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
	Effect    string   `json:"effect"`
}

// OpenCodeCalls reads one evaluate event. Only the shell action is judged;
// any other gets no opinion (ok true, no calls). A shell event with no
// resource, or one that is not a JSON object, is ok false. Several resources
// are each judged, and are never pre-approved together: an allow needs one
// resource, the whole command.
func OpenCodeCalls(payload []byte) (calls []parity.Call, ok bool) {
	var in OpenCodeDecideInput
	var probe map[string]json.RawMessage
	if json.Unmarshal(payload, &probe) != nil || probe == nil || json.Unmarshal(payload, &in) != nil {
		return nil, false
	}
	if in.Action != "shell" {
		return nil, true
	}
	if len(in.Resources) == 0 {
		return nil, false
	}
	for _, r := range in.Resources {
		if r == "" {
			return nil, false
		}
		calls = append(calls, parity.Call{Kind: parity.CallShell, Text: r})
	}
	return calls, true
}

// JudgeOpenCode is Judge for OpenCode: several resources may ask or deny, but
// never allow, since no single resource is the whole command.
func JudgeOpenCode(plan DecidePlan, calls []parity.Call, ok bool) parity.CallDecision {
	d := Judge(plan, calls, "", ok)
	if d.Verdict == parity.VerdictAllow && len(calls) != 1 {
		d.Verdict, d.Reason = parity.VerdictAsk, parity.ReasonPolicyMatch
	}
	return d
}

// OpenCodeOutput is the one word the plugin reads: allow, ask, deny, or none.
func OpenCodeOutput(d parity.CallDecision) []byte {
	if d.Verdict == parity.VerdictNone {
		return []byte("none\n")
	}
	return []byte(string(d.Verdict) + "\n")
}
