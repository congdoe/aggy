package policyhook

import (
	"encoding/json"
	"path/filepath"
	"slices"

	"github.com/congdoe/aggy/internal/parity"
)

// Antigravity's PreToolUse (agy 1.5.5, live, docs/verification.md): the
// payload carries toolCall {id, name, args}, among keys the hook never reads
// (lastUserInput is the person's prompt). The shell is run_command, its
// command args.CommandLine (live). read_file exists (live, its args not
// seen). The other tools and their argument names are from Google's hooks
// page (antigravity.google/docs/hooks, "tools", read 2026-11-11, not run):
// view_file {AbsolutePath}, write_to_file, replace_file_content, and
// multi_replace_file_content {TargetFile}, grep_search {SearchPath},
// find_by_name {SearchDirectory}, list_dir {DirectoryPath}, and manage_task
// {Action, Input}, whose send_input types into a running command.
//
// agy has its own permission layer, which prompts by default (live), so a
// tool aggy does not know gets no opinion, as on Claude: deny is
// {"decision":"deny"} and an ask {"decision":"force_ask"}, which prompted
// even where an allow rule matched, where a plain "ask" did not (live).

// agyCall is the part of the payload the hook reads.
type agyCall struct {
	ToolCall *struct {
		Name string                     `json:"name"`
		Args map[string]json.RawMessage `json:"args"`
	} `json:"toolCall"`
}

// AntigravityCalls reads one PreToolUse payload into the calls to judge. ok is
// false for a payload that is not a JSON object with a toolCall naming a tool,
// or a shell or write call whose field is missing or not a string: those ask.
// A read or search tool whose field is missing gets no opinion, since its
// argument names are from the docs only (read_file's are not even there).
func AntigravityCalls(payload []byte) (calls []parity.Call, ok bool) {
	var in agyCall
	var probe map[string]json.RawMessage
	if json.Unmarshal(payload, &probe) != nil || probe == nil || json.Unmarshal(payload, &in) != nil ||
		in.ToolCall == nil || in.ToolCall.Name == "" {
		return nil, false
	}
	args := in.ToolCall.Args
	field := func(k string) (string, bool) {
		raw, has := args[k]
		if !has {
			return "", false
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		return s, true
	}
	// must judges key as kind, asking when it cannot be read.
	must := func(kind parity.CallKind, key string, other bool) ([]parity.Call, bool) {
		s, has := field(key)
		if !has || s == "" {
			return nil, false
		}
		return []parity.Call{{Kind: kind, Text: s, Other: other}}, true
	}
	// may judges key as kind where it is there, else no opinion.
	may := func(kind parity.CallKind, key string) ([]parity.Call, bool) {
		if _, present := args[key]; !present {
			return nil, true
		}
		return must(kind, key, false)
	}
	switch in.ToolCall.Name {
	case "run_command":
		return must(parity.CallShell, "CommandLine", false)
	case "read_file", "view_file":
		return may(parity.CallRead, "AbsolutePath")
	case "write_to_file", "replace_file_content", "multi_replace_file_content":
		return must(parity.CallWrite, "TargetFile", false)
	case "grep_search":
		return may(parity.CallSearch, "SearchPath")
	case "find_by_name":
		return may(parity.CallSearch, "SearchDirectory")
	case "list_dir":
		return may(parity.CallSearch, "DirectoryPath")
	case "manage_task":
		if a, _ := field("Action"); a != "send_input" {
			return nil, true
		}
		if _, present := args["Input"]; !present {
			return nil, true
		}
		return must(parity.CallShell, "Input", true)
	}
	// A tool aggy does not know by name: judged by a field it knows, never
	// allowed (Call.Other), else no opinion.
	if _, present := args["CommandLine"]; present {
		return must(parity.CallShell, "CommandLine", true)
	}
	for _, k := range []string{"TargetFile", "AbsolutePath"} {
		if _, present := args[k]; present {
			return must(parity.CallWrite, k, true)
		}
	}
	return nil, true
}

// JudgeAntigravity decides one PreToolUse payload by the plan. An allow is no
// output (agy's own layer then asks as it would), so a pre-approval only keeps
// aggy from asking. A file tool's path that is not absolute, or lies outside
// the plan's root, asks short of a deny, as on Hermes: whether agy's own layer
// asks for files outside its workspace is unverified, so aggy does not count
// on it.
func JudgeAntigravity(plan DecidePlan, payload []byte) parity.CallDecision {
	calls, ok := AntigravityCalls(payload)
	d := Judge(plan, calls, "", ok)
	if ok && (d.Verdict == parity.VerdictNone || d.Verdict == parity.VerdictAllow) &&
		slices.ContainsFunc(calls, func(c parity.Call) bool { return c.Kind != parity.CallShell && agyOutside(c.Text, plan.Root) }) {
		d = parity.CallDecision{Verdict: parity.VerdictAsk, Group: d.Group, Strict: d.Strict, Reason: parity.ReasonOutside}
	}
	return d
}

// agyOutside says a file tool's path is not an absolute path at or under root
// (agy's file tools take absolute paths, from the docs only).
func agyOutside(p, root string) bool {
	return !filepath.IsAbs(p) || root == "" || outsideRoot(p, root)
}

// AntigravityJudged reports whether the hook would judge the payload's call at
// all, for PlanFailed.
func AntigravityJudged(payload []byte) bool {
	calls, ok := AntigravityCalls(payload)
	return !ok || len(calls) > 0
}

// reasonProtectedAgy replaces reasonProtected on Antigravity.
const reasonProtectedAgy = "this touches aggy's own files, agy's hooks or settings, or a Claude settings file, so it asks: approve this yourself"

// AntigravityOutput is the hook's stdout for d: {"decision":"deny",...} or
// {"decision":"force_ask",...} with a fixed reason, or nothing for an allow
// or no opinion.
func AntigravityOutput(d parity.CallDecision) []byte {
	var decision string
	switch d.Verdict {
	case parity.VerdictDeny:
		decision = "deny"
	case parity.VerdictAsk:
		decision = "force_ask"
	default:
		return nil
	}
	why := reasonText(d)
	if d.Reason == parity.ReasonProtected && d.Verdict == parity.VerdictAsk {
		why = reasonProtectedAgy
	}
	b, _ := json.Marshal(map[string]string{"decision": decision, "reason": why})
	return append(b, '\n')
}

// AntigravityFallback is the hook's answer when aggy cannot decide (a panic, a
// plan it cannot read): ask, as Claude's wrapper falls back to asking.
func AntigravityFallback() []byte {
	return AntigravityOutput(parity.CallDecision{Verdict: parity.VerdictAsk, Reason: parity.ReasonPlanChanged})
}
