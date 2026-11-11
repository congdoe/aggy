package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/congdoe/aggy/internal/hook"
	"github.com/congdoe/aggy/internal/permissions"
)

// Settings renders the policy's decisions as the --settings file aggy passes:
// {"permissions": {"ask": [...], "deny": [...]}}, in agy's action(target)
// rule syntax (DOCS: permissions.allow, ask, and deny, deny before ask before
// allow; LIVE: --settings with permissions.allow ["command(touch)"] was
// honoured, a file path works, and an unknown key is refused at start with
// exit 1). A Bash rule becomes command(<its text>) and a Read rule
// read_file(**/<name>). Whether agy honours deny and ask from --settings, and
// whether a * in a target is a wildcard there, is unverified: aggy's hook is
// the guard these back up. Nothing here allows anything.
func Settings(ds []Rule) ([]byte, error) {
	var ask, deny []string
	for _, d := range ds {
		r, ok := rule(d.Text)
		if !ok {
			continue
		}
		switch d.Kind {
		case permissions.Ask:
			ask = append(ask, r)
		case permissions.Deny:
			deny = append(deny, r)
		}
	}
	type perms struct {
		Ask  []string `json:"ask,omitempty"`
		Deny []string `json:"deny,omitempty"`
	}
	return json.Marshal(struct {
		Permissions perms `json:"permissions"`
	}{perms{Ask: ask, Deny: deny}})
}

// Rule is one policy decision as Settings takes it: a rule in Claude's
// spelling and its kind (parity.PolicyDecision's two fields, copied so this
// package does not import parity, which imports the manifest that imports
// this package's StripHooks).
type Rule struct {
	Text string
	Kind permissions.Kind
}

// rule is a policy rule in Claude's spelling as an agy rule.
func rule(r string) (string, bool) {
	if c, ok := strings.CutPrefix(r, "Bash("); ok {
		return "command(" + c, true
	}
	if f, ok := strings.CutPrefix(r, "Read(//**/"); ok {
		return "read_file(**/" + f, true
	}
	return "", false
}

// HooksFile is the workspace hooks file, relative to the repo root (LIVE: agy
// loads it in a trusted folder; DOCS: only once the folder is trusted).
// --settings cannot carry hooks (LIVE: "Error: --settings: unknown setting
// \"hooks\""), and the global ~/.gemini/config/hooks.json is shared with
// Google's desktop app and IDE (DOCS), so aggy writes here only.
const HooksFile = ".agents/hooks.json"

// EntryName is aggy's one named entry in the hooks file.
const EntryName = "aggy"

// Events are the hook events aggy's entry runs on: SessionStart (a new
// conversation's first turn; not on a resume, LIVE), PreToolUse (before each
// tool call, LIVE), and Stop (once per turn, fullyIdle true, LIVE).
var Events = []string{"SessionStart", "PreToolUse", "Stop"}

// Hook timeouts, in seconds. agy blocks the tool when PreToolUse runs past
// its timeout (LIVE, fail closed); the docs' default is 30 s.
const (
	PreToolTimeout = 10
	EventTimeout   = 5
)

// HookCommand is the command aggy's entry runs for event: aggy's absolute
// path and fixed words, so the entry is the same at every launch. It needs no
// quoting (hook.CheckBin), so it runs alike whether agy splits it or hands it
// to a shell (which of the two is unverified).
func HookCommand(bin, event string) (string, error) {
	if err := hook.CheckBin(bin); err != nil {
		return "", err
	}
	if !slices.Contains(Events, event) {
		return "", fmt.Errorf("antigravity: %q is not an event aggy hooks", event)
	}
	return bin + " hook antigravity --event " + event, nil
}

// handler is one command handler (LIVE shape).
type handler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// Entry is aggy's entry, as the value of EntryName: SessionStart and Stop
// take handlers, PreToolUse matcher groups (LIVE shape), here ".*", every
// tool.
func Entry(bin string) ([]byte, error) {
	cmd := map[string]string{}
	for _, e := range Events {
		c, err := HookCommand(bin, e)
		if err != nil {
			return nil, err
		}
		cmd[e] = c
	}
	type group struct {
		Matcher string    `json:"matcher"`
		Hooks   []handler `json:"hooks"`
	}
	return json.Marshal(struct {
		SessionStart []handler `json:"SessionStart"`
		PreToolUse   []group   `json:"PreToolUse"`
		Stop         []handler `json:"Stop"`
	}{
		SessionStart: []handler{{"command", cmd["SessionStart"], EventTimeout}},
		PreToolUse:   []group{{".*", []handler{{"command", cmd["PreToolUse"], PreToolTimeout}}}},
		Stop:         []handler{{"command", cmd["Stop"], EventTimeout}},
	})
}

// ErrHooksFile matches a hooks file aggy will not merge into or take its
// entry out of: not one JSON object, or one with aggy's name twice.
var ErrHooksFile = errors.New("the hooks file is not one JSON object aggy can merge into")

// member is one top-level member's place in a file: from its key's opening
// quote to the end of its value.
type member struct {
	key                        string
	keyStart, valStart, valEnd int
}

// members finds the top-level object's members, and where its braces are.
func members(src []byte) (open, end int, ms []member, err error) {
	bad := func(why string) (int, int, []member, error) {
		return 0, 0, nil, fmt.Errorf("%w: %s", ErrHooksFile, why)
	}
	dec := json.NewDecoder(bytes.NewReader(src))
	tok, err := dec.Token()
	if d, ok := tok.(json.Delim); err != nil || !ok || d != '{' {
		return bad("it is not a JSON object")
	}
	open = int(dec.InputOffset()) - 1
	for dec.More() {
		prev := int(dec.InputOffset())
		tok, err := dec.Token()
		k, ok := tok.(string)
		if err != nil || !ok {
			return bad("a key is not a string")
		}
		ks := bytes.IndexByte(src[prev:], '"')
		if ks < 0 {
			return bad("a key could not be placed")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return bad("a value is not JSON")
		}
		ve := int(dec.InputOffset())
		ms = append(ms, member{key: k, keyStart: prev + ks, valStart: ve - len(raw), valEnd: ve})
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return bad("the object does not end")
	}
	end = int(dec.InputOffset()) - 1
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bad("there is text after the object")
	}
	return open, end, ms, nil
}

// find is the index of aggy's member, -1 for none; twice is an error.
func find(ms []member) (int, error) {
	at := -1
	for i, m := range ms {
		if m.key == EntryName {
			if at >= 0 {
				return 0, fmt.Errorf("%w: %q is there twice", ErrHooksFile, EntryName)
			}
			at = i
		}
	}
	return at, nil
}

// SetHooks returns src with aggy's entry set to entry and every other byte as
// it was; nil src (no file) is a file holding aggy's entry alone. Applied to
// its own output it changes nothing.
func SetHooks(src, entry []byte) ([]byte, error) {
	if !json.Valid(entry) {
		return nil, errors.New("antigravity: aggy's hooks entry is not JSON")
	}
	if src == nil {
		return []byte("{\n  \"" + EntryName + "\": " + string(entry) + "\n}\n"), nil
	}
	open, end, ms, err := members(src)
	if err != nil {
		return nil, err
	}
	at, err := find(ms)
	if err != nil {
		return nil, err
	}
	var out []byte
	switch {
	case at >= 0:
		m := ms[at]
		out = slices.Concat(src[:m.valStart], entry, src[m.valEnd:])
	case len(ms) == 0:
		out = slices.Concat(src[:open+1], []byte("\n  \""+EntryName+"\": "), entry, []byte("\n"), src[end:])
	default:
		last := ms[len(ms)-1].valEnd
		out = slices.Concat(src[:last], []byte(",\n  \""+EntryName+"\": "), entry, src[last:])
	}
	return out, nil
}

// StripHooks returns src without aggy's entry, every other byte as it was. A
// file without it is returned as it is.
func StripHooks(src []byte) ([]byte, error) {
	open, end, ms, err := members(src)
	if err != nil {
		return nil, err
	}
	at, err := find(ms)
	if err != nil || at < 0 {
		return src, err
	}
	switch {
	case len(ms) == 1:
		return slices.Concat(src[:open+1], src[end:]), nil
	case at < len(ms)-1:
		return slices.Concat(src[:ms[at].keyStart], src[ms[at+1].keyStart:]), nil
	}
	return slices.Concat(src[:ms[at-1].valEnd], src[ms[at].valEnd:]), nil
}

// HasEntry reports whether src holds aggy's entry exactly as entry, compared
// as JSON values.
func HasEntry(src, entry []byte) bool {
	_, _, ms, err := members(src)
	if err != nil {
		return false
	}
	at, err := find(ms)
	if err != nil || at < 0 {
		return false
	}
	var a, b any
	if json.Unmarshal(src[ms[at].valStart:ms[at].valEnd], &a) != nil || json.Unmarshal(entry, &b) != nil {
		return false
	}
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// Reverse names, on a manifest row, the structural undo of aggy's entry in a
// hooks file (StripHooks): the person's other entries stay as they are.
const Reverse = "antigravity-hooks"
