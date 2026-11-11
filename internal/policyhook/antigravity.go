package policyhook

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

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
// or a call aggy cannot judge: those ask.
//
// The argument keys are from the docs only, but run_command's (LIVE), so a
// known tool whose expected key is missing is not let through (#141 review,
// HIGH-1): a read or search tool has every path-like string in its arguments
// (pathStrings, nested ones too) judged as its kind, and asks when there is
// none; a write tool missing TargetFile asks. A tool aggy does not know by
// name is judged by a field it knows, and otherwise has each path-like string
// judged as both a read and a write, never allowed (Call.Other). With no
// path-like string at all it gets no opinion, as Claude's judge gives a tool
// it does not know (ClaudeCalls): agy's own permission layer prompts by
// default (LIVE for run_command; for other tools unverified). Hermes' judge
// asks instead, since Hermes has no such layer for its other tools.
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
	// paths judges key as kind where it is there; else every path-like
	// string in the arguments, asking when there is none.
	paths := func(kind parity.CallKind, key string) ([]parity.Call, bool) {
		if _, present := args[key]; present {
			return must(kind, key, false)
		}
		ps, ok := pathStrings(args)
		if !ok || len(ps) == 0 {
			return nil, false
		}
		for _, p := range ps {
			calls = append(calls, parity.Call{Kind: kind, Text: p})
		}
		return calls, true
	}
	switch in.ToolCall.Name {
	case "run_command":
		return must(parity.CallShell, "CommandLine", false)
	case "read_file", "view_file":
		return paths(parity.CallRead, "AbsolutePath")
	case "write_to_file", "replace_file_content", "multi_replace_file_content":
		return must(parity.CallWrite, "TargetFile", false)
	case "grep_search":
		return paths(parity.CallSearch, "SearchPath")
	case "find_by_name":
		return paths(parity.CallSearch, "SearchDirectory")
	case "list_dir":
		return paths(parity.CallSearch, "DirectoryPath")
	case "manage_task":
		if a, _ := field("Action"); a != "send_input" {
			return nil, true
		}
		if _, present := args["Input"]; !present {
			return nil, true
		}
		return must(parity.CallShell, "Input", true)
	}
	if _, present := args["CommandLine"]; present {
		return must(parity.CallShell, "CommandLine", true)
	}
	for _, k := range []string{"TargetFile", "AbsolutePath"} {
		if _, present := args[k]; present {
			return must(parity.CallWrite, k, true)
		}
	}
	ps, ok := pathStrings(args)
	if !ok {
		return nil, false
	}
	for _, p := range ps {
		calls = append(calls, parity.Call{Kind: parity.CallRead, Text: p, Other: true}, parity.Call{Kind: parity.CallWrite, Text: p, Other: true})
	}
	return calls, true
}

// maxAgyStrings caps the strings pathStrings reads; more is not judged (ok
// false), so the call asks.
const maxAgyStrings = 256

// pathStrings is every string in args, at any depth (objects and arrays),
// that looks like a file path: no line break, not a URL, and starting with
// /, ~, or ., or holding a /. ok is false when args hold more strings than
// maxAgyStrings.
func pathStrings(args map[string]json.RawMessage) (out []string, ok bool) {
	n := 0
	var walk func(v any) bool
	walk = func(v any) bool {
		switch x := v.(type) {
		case string:
			if n++; n > maxAgyStrings {
				return false
			}
			if pathLike(x) {
				out = append(out, x)
			}
		case []any:
			for _, e := range x {
				if !walk(e) {
					return false
				}
			}
		case map[string]any:
			for _, e := range x {
				if !walk(e) {
					return false
				}
			}
		}
		return true
	}
	// Keys in a fixed order, so the calls do not depend on map order.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		var v any
		if json.Unmarshal(args[k], &v) != nil {
			return nil, false
		}
		if !walk(v) {
			return nil, false
		}
	}
	return out, true
}

// pathLike says s looks like a file path to judge.
func pathLike(s string) bool {
	if s == "" || strings.ContainsAny(s, "\n\r") || strings.Contains(s, "://") {
		return false
	}
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") || strings.HasPrefix(s, ".") || strings.Contains(s, "/")
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
	if ok && (d.Verdict == parity.VerdictNone || d.Verdict == parity.VerdictAllow) &&
		slices.ContainsFunc(calls, func(c parity.Call) bool { return c.Kind == parity.CallShell && agyReach(c.Text) }) {
		d = parity.CallDecision{Verdict: parity.VerdictAsk, Group: d.Group, Strict: d.Strict, Reason: parity.ReasonProtected}
	}
	return d
}

// agyExpansion is a shell expansion inside a word: ${...}, $(...), $name, or
// a backquoted command.
var agyExpansion = regexp.MustCompile("\\$\\{[^}]*\\}|\\$\\([^)]*\\)|\\$[A-Za-z0-9_]+|`[^`]*`")

// agyReach reports a shell command with a word that could reach the launch
// folder's .agents without spelling it (#141 review, M3), as the key-folder
// rule does for aggy's state folder: a path part that starts with a dot and
// holds a wildcard matching ".agents" (.ag*, .a?ents, .*), or one built
// around an expansion whose literal piece is three or more letters of
// ".agents" at its start or end (${d}ents, .ag$x). A plain * does not count,
// since a shell's * skips dot files, so ls * and rm src/*.go do not ask. A
// command that names .agents or hooks.json is caught by the plan's words.
// What this misses (find . -delete, a script) is what a model's shell can do
// anyway: aggy checks the file at each turn end and launch.
func agyReach(cmd string) bool {
	words := strings.FieldsFunc(cmd, func(r rune) bool { return strings.ContainsRune(" \t\n;|&()<>'\"=", r) })
	for _, w := range words {
		for _, part := range strings.Split(w, "/") {
			if strings.HasPrefix(part, ".") && strings.ContainsAny(part, "*?[") {
				if m, _ := filepath.Match(part, ".agents"); m {
					return true
				}
			}
			if !strings.ContainsAny(part, "$`") {
				continue
			}
			for _, piece := range agyExpansion.Split(part, -1) {
				if len(piece) >= 3 && (strings.HasPrefix(".agents", piece) || strings.HasSuffix(".agents", piece)) {
					return true
				}
			}
		}
	}
	return false
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
