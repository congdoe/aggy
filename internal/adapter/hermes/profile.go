package hermes

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The block aggy owns in its profile's config.yaml. Hermes reads the
// profile's config.yaml merged over its defaults (hermes_cli/config.py:1978,
// load_config: DEFAULT_CONFIG + config.yaml), so a key aggy leaves out keeps
// its default. Hermes itself rewrites the file with comments kept
// (config.py:1957-1967, "ruamel comment preservation"), so the block is
// found again by its two marker lines.

// BlockStart and BlockEnd are the marker lines around aggy's block.
const (
	BlockStart = "# >>> aggy: this block is aggy's; aggy writes it at each Hermes launch here and aggy unwire takes it out"
	BlockEnd   = "# <<< aggy"
)

// OwnedKeys are the top-level keys aggy's block sets. One of them outside
// the block would be a second copy whose effect aggy cannot tell, so the
// launch is refused.
var OwnedKeys = []string{"approvals", "hooks", "hooks_auto_accept", "updates", "auth"}

// The hook events aggy configures (hermes_cli/plugins.py:111-156, VALID_HOOKS):
// pre_tool_call, the policy decision before each tool call, the only event a
// hook can block (agent/shell_hooks.py:44, _BLOCKING_EVENTS); and
// on_session_end, which Hermes fires at the end of every turn, despite its
// name, with completed, failed, and interrupted (agent/turn_finalizer.py:
// 882-896: "run_conversation() runs once per message"; LIVE, its extra had
// those keys). post_llm_call is not configured: it fires only once a reply
// exists (turn_finalizer.py:470-500), and its extra holds the conversation.
const (
	EventPreToolCall  = "pre_tool_call"
	EventSessionEnd   = "on_session_end"
	PreToolTimeout    = 10 // seconds; Hermes' default is 60, max 300 (shell_hooks.py:37-38)
	SessionEndTimeout = 5
)

// Events lists them in the order the block writes them.
var Events = []string{EventPreToolCall, EventSessionEnd}

// HookArgs are the arguments after aggy's binary in a hook's command:
// `hook hermes --home <aggy's state folder> --profile <p> --event <e>`. A
// hook runs in Hermes' daemon, which passes no AGGY_* variable, so the state
// folder goes on the command. They are the same at every launch in a repo,
// so Hermes' consent, which keys on the (event, command) pair and is kept per
// profile (shell_hooks.py:703-705, :787-790), is asked once.
func HookArgs(home, profile, event string) []string {
	return []string{"hook", "hermes", "--home", home, "--profile", profile, "--event", event}
}

// safeWord is what a hook command's words may hold: Hermes splits the
// command like a shell (shell_hooks.py:493, split_command_line, after
// expanduser) and runs it with no shell, so no word may need quoting.
var safeWord = regexp.MustCompile(`^[A-Za-z0-9/._@+-]+$`)

// HookCommand is a hook's command string.
func HookCommand(bin, home, profile, event string) (string, error) {
	for _, p := range []string{bin, home} {
		if !strings.HasPrefix(p, "/") || !safeWord.MatchString(p) {
			return "", fmt.Errorf("hermes: the path %q cannot go in a hook command unquoted", p)
		}
	}
	if !ValidProfile(profile) || !slices.Contains(Events, event) {
		return "", errors.New("hermes: bad hook profile or event")
	}
	return bin + " " + strings.Join(HookArgs(home, profile, event), " "), nil
}

// Block is what aggy's block sets.
type Block struct {
	// Bin is aggy's absolute path, and Home its state folder, for the hooks.
	Bin, Home string
	Profile   string
	// Deny are approvals.deny globs (parity.HermesDeny): Hermes matches them,
	// case-insensitively and with fnmatch, against terminal commands only,
	// before yolo or mode off (tools/approval_floors.py:23-42,
	// tools/approval.py:1189-1212).
	Deny []string
}

// Render returns the block, markers included, ending in a newline.
//
//   - approvals.mode manual: a person answers each flagged command, never the
//     auxiliary model "smart" asks (config_defaults.py:1646-1660); cron,
//     single-query, and unattended runs deny (their defaults, set again).
//   - hooks: pre_tool_call with fail_closed, so a hook that fails, hangs, or
//     prints no directive blocks the call (shell_hooks.py:560-616), and
//     on_session_end; hooks_auto_accept false, so Hermes asks the person once
//     for each (shell_hooks.py:816-823).
//   - updates.check false: no network update check at launch
//     (hermes_cli/source_check.py:570).
//   - auth.adopt_external_logins false: this profile never reads or refreshes
//     Codex's or Claude Code's logins (config_defaults.py:1733-1739).
func (b Block) Render() ([]byte, error) {
	var w bytes.Buffer
	line := func(indent int, s string) { w.WriteString(strings.Repeat(" ", indent) + s + "\n") }
	line(0, BlockStart)
	line(0, "approvals:")
	line(2, "mode: manual")
	line(2, "cron_mode: deny")
	line(2, "single_query_mode: deny")
	line(2, "unattended_mode: deny")
	if len(b.Deny) == 0 {
		line(2, "deny: []")
	} else {
		line(2, "deny:")
		for _, g := range b.Deny {
			q, err := quote(g)
			if err != nil {
				return nil, err
			}
			line(4, "- "+q)
		}
	}
	line(0, "hooks:")
	for _, ev := range Events {
		cmd, err := HookCommand(b.Bin, b.Home, b.Profile, ev)
		if err != nil {
			return nil, err
		}
		q, _ := quote(cmd)
		line(2, ev+":")
		line(4, "- command: "+q)
		if ev == EventPreToolCall {
			line(6, "timeout: "+strconv.Itoa(PreToolTimeout))
			line(6, "fail_closed: true")
		} else {
			line(6, "timeout: "+strconv.Itoa(SessionEndTimeout))
		}
	}
	line(0, "hooks_auto_accept: false")
	line(0, "updates:")
	line(2, "check: false")
	line(0, "auth:")
	line(2, "adopt_external_logins: false")
	line(0, BlockEnd)
	return w.Bytes(), nil
}

// quote is s as a YAML double-quoted scalar. Only printable text without a
// double quote or backslash is taken, so nothing needs an escape.
func quote(s string) (string, error) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return "", fmt.Errorf("hermes: %q cannot go in aggy's block", s)
		}
	}
	return `"` + s + `"`, nil
}

// ErrBlock is a config.yaml whose aggy block cannot be found or replaced
// safely: markers out of order, or a key aggy sets outside them.
var ErrBlock = errors.New("hermes: the profile's config.yaml does not hold aggy's block in a form aggy can replace")

// SetBlock returns src with aggy's block replaced by block, or added at the
// end. It is deterministic and idempotent, as patch.Patch.Transform must be.
func SetBlock(src, block []byte) ([]byte, error) {
	rest, err := StripBlock(src)
	if err != nil {
		return nil, err
	}
	for _, l := range strings.Split(string(rest), "\n") {
		k, _, ok := strings.Cut(l, ":")
		if ok && slices.Contains(OwnedKeys, strings.TrimSpace(k)) && k == strings.TrimLeft(k, " \t-") {
			return nil, fmt.Errorf("%w: it sets %s outside aggy's block", ErrBlock, k)
		}
	}
	out := slices.Clone(rest)
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(out, block...), nil
}

// StripBlock returns src without aggy's block: what aggy unwire leaves.
// No block returns src as it is.
func StripBlock(src []byte) ([]byte, error) {
	lines := strings.SplitAfter(string(src), "\n")
	start, end := -1, -1
	for i, l := range lines {
		switch strings.TrimRight(l, "\r\n") {
		case BlockStart:
			if start >= 0 {
				return nil, fmt.Errorf("%w: two start lines", ErrBlock)
			}
			start = i
		case BlockEnd:
			if end >= 0 || start < 0 {
				return nil, fmt.Errorf("%w: an end line out of place", ErrBlock)
			}
			end = i
		}
	}
	switch {
	case start < 0 && end < 0:
		return src, nil
	case start < 0 || end < 0:
		return nil, fmt.Errorf("%w: a start line with no end", ErrBlock)
	}
	return []byte(strings.Join(lines[:start], "") + strings.Join(lines[end+1:], "")), nil
}

// Reverse is the manifest's structural reverse for aggy's block
// (manifest.reverserFor).
const Reverse = "hermes-block"
