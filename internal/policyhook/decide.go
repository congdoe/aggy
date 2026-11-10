// Package policyhook is aggy's policy decision hook (T6976, docs/design.md,
// section 10): aggy hook decide reads one tool call from a vendor's hook and
// answers from the launch's DecidePlan, a file aggy wrote for that launch and
// pinned by its SHA-256 in the hook's own command, through parity's one
// decision (PolicyPlan.DecideCall). Nothing in the payload but the tool input
// decides; the session's own permission mode may only turn an ask into a deny.
// It also builds the hook's command for Claude, with its fixed fallback. The
// only file it reads is the one a read pre-approval names, through safefs, to
// check it is still the file the person approved.
package policyhook

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/safefs"
)

// DecidePlanVersion is the plan format this aggy reads.
const DecidePlanVersion = 2

// MaxDecidePlan caps the plan file.
const MaxDecidePlan = 1 << 20

// DecidePlan is what a launch tells its decision hook, in a file in the
// launch's private folder that only the hook command's argv names, beside its
// SHA-256.
type DecidePlan struct {
	Version int    `json:"version"`
	Vendor  string `json:"vendor"`
	// VendorVersion is the CLI's version from the probe cache, or "unknown",
	// for the log.
	VendorVersion string `json:"vendor_version"`
	// NoPrompt is a session that cannot show a prompt (bypassPermissions or
	// dontAsk however set at launch): every ask is a deny.
	NoPrompt bool `json:"no_prompt"`
	// Root is the launch's repo root (its folder, outside a repo), absolute:
	// a relative path in a call is taken from here, never from the cwd the
	// payload names, and a read pre-approval must lie under it.
	Root string `json:"root"`
	// Protected are absolute paths whose writes, and whose naming in a shell
	// command, always ask (PR #129 review): aggy's state folder, which holds
	// this plan, the run folders, and aggy's permissions file.
	Protected []string `json:"protected,omitempty"`
	// Settings are the person's group settings, ask or deny.
	Settings map[string]string `json:"settings,omitempty"`
	// Branches are default branches besides parity.ProtectedBranches.
	Branches []string `json:"branches,omitempty"`
	// Commands and Reads are the exact pre-approvals (parity.PreApprovedFrom).
	Commands []string              `json:"commands,omitempty"`
	Reads    []parity.ReadApproval `json:"reads,omitempty"`
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

// PlanSum is the hex SHA-256 of a plan file's bytes, which the launch puts in
// the hook's command and the hook checks the file against.
func PlanSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ValidSum reports a well-formed PlanSum: 64 lower-case hex digits.
func ValidSum(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !strings.ContainsRune("0123456789abcdef", rune(s[i])) {
			return false
		}
	}
	return true
}

// ErrPlanChanged is a plan file whose bytes are not the ones the launch
// wrote: something changed it during the session.
var ErrPlanChanged = errors.New("the decision plan is not the one this launch wrote")

// CheckPlan parses b as the plan whose PlanSum is sum. Bytes that do not
// hash to sum are ErrPlanChanged, whatever they hold.
func CheckPlan(b []byte, sum string) (DecidePlan, error) {
	if !ValidSum(sum) || PlanSum(b) != sum {
		return DecidePlan{}, ErrPlanChanged
	}
	return ParseDecidePlan(b)
}

// ParseDecidePlan reads a plan: one JSON object of this version, no unknown
// key, an absolute root, and only ask or deny for a known group. Anything
// else is an error, and the hook asks.
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
	if !filepath.IsAbs(d.Root) || filepath.Clean(d.Root) != d.Root {
		return DecidePlan{}, errors.New("hook: the decision plan's root is not an absolute, clean path")
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

// PreApproved is the plan's exact pre-approvals, with the disk check a read
// pre-approval needs.
func (d DecidePlan) PreApproved() parity.PreApproved {
	return parity.PreApproved{Commands: d.Commands, Reads: d.Reads, CheckRead: d.sameFile}
}

// lstat is safefs.Lstat; a test swaps it.
var lstat = safefs.Lstat

// sameFile says a read pre-approval's file is still the one the person
// approved (PR #129 review, MEDIUM-4): it lies under the plan's root, no part
// of its path below the root is a symlink, and it is a regular file with the
// device and inode recorded when it was added. A replaced file, a link in its
// place or on the way, or anything it cannot check, asks again.
func (d DecidePlan) sameFile(a parity.ReadApproval) bool {
	dev, ino, err := FileIdentity(d.Root, a.Path)
	return err == nil && dev == a.Dev && ino == a.Ino
}

// FileIdentity is the device and inode of the regular file at p, which must
// be absolute, clean, and under root, with no symlink at any part of its path
// below root: what aggy permissions add records for a read pre-approval, and
// what the hook compares against. Anything else is an error.
func FileIdentity(root, p string) (dev, ino uint64, err error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return 0, 0, errors.New("not an absolute, clean path under the repo")
	}
	rel, err := filepath.Rel(filepath.Clean(root), p)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return 0, 0, errors.New("not under the repo")
	}
	cur := filepath.Clean(root)
	parts := strings.Split(rel, string(filepath.Separator))
	for i, name := range parts {
		cur = filepath.Join(cur, name)
		fi, err := lstat(cur)
		if err != nil {
			return 0, 0, err
		}
		last := i == len(parts)-1
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			return 0, 0, errors.New("a symlink is on its path")
		case !last && !fi.IsDir():
			return 0, 0, errors.New("not a folder on its path")
		case last:
			dev, ino, ok := safefs.FileID(fi)
			if !ok || !fi.Mode().IsRegular() {
				return 0, 0, errors.New("not a regular file")
			}
			return dev, ino, nil
		}
	}
	return 0, 0, errors.New("no file")
}

// ClaudeHookTools are the Claude Code tools aggy's decision hook judges by
// name: the shell, Monitor (which runs a shell command and watches its output:
// its command input is read from Claude Code's own tool definition, as a
// session lists it; live at 2.1.322, a PreToolUse hook sees tool_name Monitor,
// docs/verification.md), and every
// built-in tool a Read rule applies to, which permissions.md names as Read,
// Edit (with Write and the other editing tools) and, best effort, Grep and
// Glob ("Read and Edit", read 2026-11-10). The hook is matched to every tool
// (ClaudeHookMatcher), so a tool not listed is still judged by its input
// (ClaudeCalls).
var ClaudeHookTools = []string{"Bash", "Monitor", "Read", "Edit", "Write", "MultiEdit", "NotebookEdit", "Grep", "Glob"}

// ClaudeHookMatcher is the hook entry's matcher: "*" matches every tool
// (hooks.md, "Structure"), so a tool aggy does not know by name, one with a
// shell command or a file path in its input, still reaches the hook
// (PR #129 review, HIGH-3).
func ClaudeHookMatcher() string { return "*" }

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

// ClaudeMode reads only the permission mode from a payload, "" if none.
func ClaudeMode(payload []byte) string {
	var in claudeToolInput
	_ = json.Unmarshal(payload, &in)
	return in.PermissionMode
}

// fileKeys are the input fields that name a file in a tool aggy does not
// know by name: it is judged as a file tool that writes.
var fileKeys = []string{"file_path", "notebook_path", "path"}

// ClaudeCalls reads one PreToolUse payload into the calls to judge, and the
// session's permission mode. ok is false for a payload that is not a JSON
// object, or lacks or mistypes the field a known tool needs: such a call is
// asked about, never allowed. A tool not known by name is judged by its
// input: a command string as a shell command, a file_path, notebook_path, or
// path as a file it may write; with none of them, no call and ok true, no
// opinion (a web fetch, a subagent, an MCP tool's other inputs).
func ClaudeCalls(payload []byte) (calls []parity.Call, mode string, ok bool) {
	var in claudeToolInput
	var probe map[string]json.RawMessage
	if json.Unmarshal(payload, &probe) != nil || probe == nil || json.Unmarshal(payload, &in) != nil || in.ToolName == "" {
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
	case "Bash", "Monitor":
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
	// A tool aggy does not know by name (PR #129 review, HIGH-3).
	if _, present := in.ToolInput["command"]; present {
		return one(parity.CallShell, "command")
	}
	for _, k := range fileKeys {
		if _, present := in.ToolInput[k]; present {
			return one(parity.CallWrite, k)
		}
	}
	return nil, in.PermissionMode, true
}

// verdictRank orders verdicts for combining several calls: deny, then ask,
// then allow, then no opinion, as Claude combines hooks (hooks.md).
var verdictRank = []parity.Verdict{parity.VerdictDeny, parity.VerdictAsk, parity.VerdictAllow, parity.VerdictNone}

// Judge decides calls by the plan. A call it could not read (ok false) asks.
// A call that writes, or a shell command that names, a protected path asks
// whatever else it matched, unless a group set to deny refuses it. A session
// that cannot show a prompt (the plan's NoPrompt, or a mode from the payload
// in noPromptModes) gets a deny for every ask. It never allows what it could
// not read.
func Judge(plan DecidePlan, calls []parity.Call, mode string, ok bool) parity.CallDecision {
	d := parity.CallDecision{Verdict: parity.VerdictAsk, Reason: parity.ReasonUnknownInput}
	if ok {
		d = parity.CallDecision{Verdict: parity.VerdictNone, Reason: parity.ReasonNoMatch}
		pol, pre := plan.Policy(), plan.PreApproved()
		for _, c := range calls {
			cd := pol.DecideCall(c, pre)
			if (cd.Verdict == parity.VerdictNone || cd.Verdict == parity.VerdictAllow) && plan.protectedCall(c) {
				cd = parity.CallDecision{Verdict: parity.VerdictAsk, Group: cd.Group, Strict: cd.Strict, Reason: parity.ReasonProtected}
			}
			if slices.Index(verdictRank, cd.Verdict) < slices.Index(verdictRank, d.Verdict) {
				d = cd
			}
		}
	}
	return noPrompt(d, plan.NoPrompt || NoPromptMode(mode))
}

// noPrompt turns an ask into a deny where no prompt can show.
func noPrompt(d parity.CallDecision, no bool) parity.CallDecision {
	if d.Verdict == parity.VerdictAsk && no {
		d.Verdict, d.Reason = parity.VerdictDeny, parity.ReasonNoPrompt
	}
	return d
}

// PlanFailed is the decision when the plan is missing, unreadable, or changed:
// ask, or deny where the payload's mode shows no prompt. Never allow.
func PlanFailed(mode string) parity.CallDecision {
	return noPrompt(parity.CallDecision{Verdict: parity.VerdictAsk, Reason: parity.ReasonPlanChanged}, NoPromptMode(mode))
}

// realPath is safefs.RealPath; a test swaps it.
var realPath = safefs.RealPath

// protectedCall reports a call that writes a protected path or a Claude
// settings file, or a shell command whose text names one (PR #129 review,
// HIGH-1 and HIGH-4). The shell check reads text, so a name assembled at run
// time gets past it: it catches mistakes and plain attempts, while the plan's
// hash is what stops a changed plan from being used.
func (d DecidePlan) protectedCall(c parity.Call) bool {
	switch c.Kind {
	case parity.CallShell:
		return protectedText(c.Text, d.Protected)
	case parity.CallWrite:
		return d.protectedPath(c.Text)
	}
	return false
}

// protectedPath reports a path that is, or is under, a protected path, or is
// a Claude settings file (.claude/settings*.json), compared cleaned, absolute
// from the plan's root, and without regard to case (a disk that folds case
// opens .CLAUDE/Settings.JSON as the same file). The real path of its folder
// is checked too, so a link to a protected folder counts.
func (d DecidePlan) protectedPath(p string) bool {
	if p == "" {
		return false
	}
	if !filepath.IsAbs(p) {
		if d.Root == "" {
			return claudeSettingsPath(p)
		}
		p = filepath.Join(d.Root, p)
	}
	p = filepath.Clean(p)
	if d.protectedClean(p) {
		return true
	}
	dir, err := realPath(filepath.Dir(p))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false
	case err != nil:
		return true // cannot tell where it leads: ask
	}
	return d.protectedClean(filepath.Join(dir, filepath.Base(p)))
}

func (d DecidePlan) protectedClean(p string) bool {
	lp := strings.ToLower(p)
	for _, q := range d.Protected {
		lq := strings.ToLower(filepath.Clean(q))
		if lp == lq || strings.HasPrefix(lp, strings.TrimSuffix(lq, "/")+"/") {
			return true
		}
	}
	return claudeSettingsPath(p)
}

// claudeSettingsPath reports a path whose last two parts are .claude and a
// settings*.json, whatever their case: the files where Claude reads
// disableAllHooks and its permission rules.
func claudeSettingsPath(p string) bool {
	parts := strings.FieldsFunc(strings.ToLower(filepath.ToSlash(p)), func(r rune) bool { return r == '/' })
	n := len(parts)
	if n < 2 || parts[n-2] != ".claude" {
		return false
	}
	ok, _ := filepath.Match("settings*.json", parts[n-1])
	return ok
}

// protectedWords are the texts that make a shell command name a protected
// file, lower case: aggy's plan file and state folders, and a Claude settings
// file's name. ".claude" with "settings" anywhere counts too, as does ".aggy"
// with "permissions" or "run".
var protectedWords = []string{"policy-decide", ".aggy/run", ".aggy/permissions", "settings.local.json", "managed-settings"}

// protectedText reports a shell command whose text, in any of the matcher's
// spellings (raw, continuations joined, quotes and backslashes gone), and
// without regard to case, names a protected path.
func protectedText(cmd string, protected []string) bool {
	joined := strings.ReplaceAll(cmd, "\\\n", "")
	bare := strings.NewReplacer(`"`, "", "'", "", `\`, "").Replace(joined)
	for _, f := range []string{cmd, joined, bare} {
		f = strings.ToLower(f)
		for _, w := range protectedWords {
			if strings.Contains(f, w) {
				return true
			}
		}
		for _, q := range protected {
			if q != "" && strings.Contains(f, strings.ToLower(filepath.Clean(q))) {
				return true
			}
		}
		if strings.Contains(f, ".claude") && strings.Contains(f, "settings") {
			return true
		}
		if strings.Contains(f, ".aggy") && (strings.Contains(f, "permissions") || strings.Contains(f, "run")) {
			return true
		}
	}
	return false
}

// The reasons Claude shows with a decision. They are fixed text and a group
// name, never anything from the tool input.
const (
	reasonAllow     = "aggy: you pre-approved this exact command with aggy permissions add allow (%s)"
	reasonAsk       = "aggy's safety policy (%s): approve this yourself"
	reasonUnread    = "aggy could not read this call, so it asks: approve this yourself"
	reasonTooLong   = "aggy does not scan a call this long, so it asks: approve this yourself"
	reasonProtected = "this touches aggy's own files or a Claude settings file, so aggy asks: approve this yourself"
	reasonPlan      = "aggy's decision plan for this session changed or could not be read, so it asks: approve this yourself"
	reasonDeny      = "aggy's safety policy refuses this: you set %s to deny (aggy permissions policy)"
	reasonNoPrompt  = "aggy's safety policy (%s) asks about this, and this session cannot show a prompt, so it is refused"
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
	case d.Reason == parity.ReasonTooLong:
		why = reasonTooLong
	case d.Reason == parity.ReasonProtected:
		why = reasonProtected
	case d.Reason == parity.ReasonPlanChanged:
		why = reasonPlan
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
//	out=$(<aggy> hook decide --vendor claude --plan <plan> --sha256 <sum>) && printf '%s' "$out" ||
//	  { <aggy> hook decide-failed ... </dev/null >/dev/null 2>&1 & printf '%s\n' '<fixed decision>'; }
//
// aggy's own answer is printed only when it exits 0, so stdout never carries
// two decisions (PR #129 review, LOW-1). When aggy exits non-zero or is
// missing, the fallback asks for every call (or denies, where no prompt can
// show), as it cannot judge the policy, and makes one attempt to record the
// failure in the background, its output sent nowhere so Claude never waits on
// it. Both paths must pass CheckBin, so neither needs quoting, and sum must be
// a PlanSum.
func DecideCommand(bin, plan, sum string, noPrompt bool) (string, error) {
	if err := hook.CheckBin(bin); err != nil {
		return "", err
	}
	if err := hook.CheckBin(plan); err != nil {
		return "", fmt.Errorf("the decision plan's path cannot go in a hook command: %w", err)
	}
	if !ValidSum(sum) {
		return "", errors.New("the decision plan's SHA-256 is not 64 hex digits")
	}
	args := " --vendor claude --plan " + plan + " --sha256 " + sum
	return "out=$(" + bin + " hook decide" + args + ") && printf '%s' \"$out\" || { " + bin + " hook decide-failed" + args +
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

// OpenCodeCalls reads one evaluate event. The shell and read actions are
// judged; any other gets no opinion (ok true, no calls). A shell or read event
// with no resource, or one that is not a JSON object, is ok false. Several
// resources are each judged, and are never pre-approved together: an allow
// needs one resource, the whole command or file. A read's relative resource is
// taken from the plan's root (unverified at 2.2.5: what a read's resource
// holds; docs/verification.md).
func OpenCodeCalls(payload []byte, root string) (calls []parity.Call, ok bool) {
	var in OpenCodeDecideInput
	var probe map[string]json.RawMessage
	if json.Unmarshal(payload, &probe) != nil || probe == nil || json.Unmarshal(payload, &in) != nil {
		return nil, false
	}
	kind := parity.CallShell
	switch in.Action {
	case "shell":
	case "read":
		kind = parity.CallRead
	default:
		return nil, true
	}
	if len(in.Resources) == 0 {
		return nil, false
	}
	for _, r := range in.Resources {
		if r == "" {
			return nil, false
		}
		if kind == parity.CallRead && !filepath.IsAbs(r) && root != "" {
			r = filepath.Join(root, r)
		}
		calls = append(calls, parity.Call{Kind: kind, Text: r})
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
