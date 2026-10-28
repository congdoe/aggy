package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/congdoe/aggy/internal/adapter"
)

// Timeout is the timeout, in seconds, aggy's hook entries carry. aggy
// hook itself gives up after a few seconds.
const Timeout = 10

// Entry is one hook aggy installs.
type Entry struct {
	Vendor adapter.Vendor
	Event  string
}

// Command is the command a hook config runs for e:
//
//	test -x <aggy> && <aggy> hook --vendor <vendor> <event> || true
//
// Every CLI hands it to a shell: Claude to sh -c (hooks.md), Codex to
// $SHELL -lc, falling back to /bin/sh (codex-rs/hooks/src/engine/command_runner.rs,
// build_command, at 6aaac694), and Grok as "an inline shell command"
// (10-hooks.md). CheckBin allows only characters no shell treats
// specially, so the path needs no quoting, and test, &&, and || mean the
// same in sh, bash, zsh, and fish 3, login shell or not. The test and
// the || true make a hook whose aggy binary is gone (uninstalled, or an
// old path) exit 0 quietly: Claude would otherwise show a "hook error"
// notice on every event (hooks.md, "Other exit codes"). aggy hook itself
// exits 0 for every forwarded event. The payload goes to stdin, never
// into the command.
func Command(bin string, e Entry) (string, error) {
	if err := CheckBin(bin); err != nil {
		return "", err
	}
	return "test -x " + bin + " && " + bin + " hook --vendor " + string(e.Vendor) + " " + e.Event + " || true", nil
}

// binChars are the characters an aggy path in a hook command may hold.
const binChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789/._+-@"

// CheckBin refuses a path aggy cannot put in a hook command as is: one
// that is not clean and absolute, or holds a character outside
// [A-Za-z0-9/._+@-] (a space, a quote, a glob, a shell or fish
// metacharacter, or a control character).
func CheckBin(bin string) error {
	if !filepath.IsAbs(bin) || filepath.Clean(bin) != bin {
		return fmt.Errorf("aggy's path %q is not a clean absolute path", bin)
	}
	if i := strings.IndexFunc(bin, func(r rune) bool { return !strings.ContainsRune(binChars, r) }); i >= 0 {
		return fmt.Errorf("aggy's path %q holds %q, which a hook command would need quoted; install aggy where its path has only letters, digits, and / . _ + @ -", bin, bin[i:i+1])
	}
	return nil
}

// ErrAmbiguous means aggy will not merge its hooks into a file, or take
// them out, because the result would not be clearly the file plus (or
// less) aggy's entries.
var ErrAmbiguous = errors.New("ambiguous hook config")

// ambiguous wraps a reason in ErrAmbiguous.
func ambiguous(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrAmbiguous}, a...)...)
}

// aggyHook reports whether cmd is a command aggy installs (Command),
// for any binary path, vendor, and event; the bare form
// "<dir>/aggy hook --vendor <v> <event>", the path quoted or not, as
// earlier builds wrote it, counts too. It returns the vendor and event.
func aggyHook(cmd string) (adapter.Vendor, string, bool) {
	if rest, ok := strings.CutPrefix(cmd, "test -x "); ok {
		p, after, ok := strings.Cut(rest, " && ")
		after, ok2 := strings.CutSuffix(after, " || true")
		if !ok || !ok2 || !strings.HasPrefix(after, p+" hook --vendor ") {
			return "", "", false
		}
		cmd = after
	}
	prog, rest, ok := strings.Cut(cmd, " hook --vendor ")
	if !ok {
		return "", "", false
	}
	v, event, ok := strings.Cut(rest, " ")
	if !ok || strings.Contains(event, " ") || !eventName(event) {
		return "", "", false
	}
	vendor, err := adapter.ParseVendor(v)
	if err != nil {
		return "", "", false
	}
	prog = strings.TrimSuffix(strings.TrimPrefix(prog, "'"), "'")
	if !filepath.IsAbs(prog) || filepath.Base(prog) != "aggy" {
		return "", "", false
	}
	return vendor, event, true
}

func eventName(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_", r) {
			return false
		}
	}
	return true
}

// Missing returns how many of want the Claude-format hooks file src
// ({"hooks": {"<Event>": [{"matcher": ..., "hooks": [{"type":
// "command", "command": ...}]}]}}) lacks or holds with another aggy
// path (an older install, which Merge updates in place). nil or empty
// src holds none. A file aggy cannot read unambiguously is an error
// (ErrAmbiguous): not one JSON object, a duplicate key, "hooks" or an
// event of the wrong type, or two aggy hooks for one vendor and event.
func Missing(src []byte, bin string, want []Entry) (int, error) {
	n := 0
	for _, e := range want {
		st, err := entryState(src, bin, e)
		if err != nil {
			return 0, err
		}
		if st.exact == nil {
			n++
		}
	}
	return n, nil
}

// state is where an entry stands in a file.
type state struct {
	root  *node
	hooks *node // the "hooks" object, or nil
	event *node // the event's list, or nil
	exact *node // aggy's command string with this bin
	stale *node // aggy's command string with another path
}

func entryState(src []byte, bin string, e Entry) (state, error) {
	cmd, err := Command(bin, e)
	if err != nil {
		return state{}, err
	}
	root, err := parse(src)
	if err != nil {
		return state{}, err
	}
	st := state{root: root}
	if root == nil {
		return st, nil
	}
	if st.hooks, err = root.member("hooks", '{'); err != nil || st.hooks == nil {
		return st, err
	}
	if st.event, err = st.hooks.member(e.Event, '['); err != nil || st.event == nil {
		return st, err
	}
	found := 0
	err = commands(st.event, e.Event, func(c *node) {
		v, ev, ok := aggyHook(c.str)
		if !ok || v != e.Vendor || ev != e.Event {
			return
		}
		found++
		if c.str == cmd {
			st.exact = c
		} else {
			st.stale = c
		}
	})
	if err == nil && found > 1 {
		err = ambiguous("hooks.%s holds %d aggy hooks for %s; remove all but one", e.Event, found, e.Vendor)
	}
	return st, err
}

// commands calls f with each command string node under an event's list.
func commands(list *node, event string, f func(*node)) error {
	for _, g := range list.vals {
		if g.kind != '{' {
			return ambiguous("an entry of hooks.%s is not an object", event)
		}
		hs, err := g.member("hooks", '[')
		if err != nil || hs == nil {
			if err != nil {
				return ambiguous("an entry of hooks.%s has hooks that are not a list", event)
			}
			continue
		}
		for _, h := range hs.vals {
			if h.kind != '{' {
				return ambiguous("a hook under hooks.%s is not an object", event)
			}
			if c, _ := h.member("command", 's'); c != nil {
				f(c)
			}
		}
	}
	return nil
}

// group is the matcher group aggy adds: no matcher (every occurrence)
// and one command hook.
type group struct {
	Hooks []groupHook `json:"hooks"`
}

type groupHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// Merge returns a patch transform that adds each of want to a
// Claude-format hooks file as its own matcher group, or updates an older
// aggy entry's path in place. It inserts text and never re-encodes the
// file: every other byte, its indentation, key order, number spelling,
// and escapes included, stays as it was. When nothing is missing it
// returns src unchanged, so it is idempotent. It checks that the result,
// less aggy's entries, means what src less aggy's entries meant.
func Merge(bin string, want []Entry) func([]byte) ([]byte, error) {
	return func(src []byte) ([]byte, error) {
		out := src
		for _, e := range want {
			st, err := entryState(out, bin, e)
			if err != nil {
				return src, err
			}
			cmd, _ := Command(bin, e)
			switch {
			case st.exact != nil:
				continue
			case st.stale != nil:
				out = splice(out, st.stale.start, st.stale.end, encodeJSON(cmd, "", ""))
				continue
			}
			g := group{Hooks: []groupHook{{Type: "command", Command: cmd, Timeout: Timeout}}}
			switch {
			case st.root == nil: // a new or empty file
				out = []byte(encodeJSON(map[string]any{"hooks": map[string][]group{e.Event: {g}}}, "", "  ") + "\n")
			case st.hooks == nil:
				out = insert(out, st.root, `"hooks"`, map[string][]group{e.Event: {g}})
			case st.event == nil:
				out = insert(out, st.hooks, encodeJSON(e.Event, "", ""), []group{g})
			default:
				out = insert(out, st.event, "", g)
			}
		}
		if bytes.Equal(out, src) {
			return src, nil
		}
		if err := sameLessAggy(src, out); err != nil {
			return src, err
		}
		if n, err := Missing(out, bin, want); err != nil || n > 0 {
			return src, ambiguous("cannot add aggy's hooks safely")
		}
		return out, nil
	}
}

// Strip removes every aggy hook command (any path, vendor, and event)
// from a Claude-format hooks file by deleting text: a matcher group that
// holds only aggy hooks goes whole, an event list left empty by that goes
// with its key, and so does a "hooks" object left empty. Everything else
// stays byte for byte. This is how aggy unwire takes aggy's hooks out of
// a file the person or the CLI has changed since. A file aggy cannot read
// unambiguously is an error.
func Strip(src []byte) ([]byte, error) {
	out := src
	for {
		root, err := parse(out)
		if err != nil {
			return src, err
		}
		next, ok, err := stripOne(out, root)
		if err != nil {
			return src, err
		}
		if !ok {
			break
		}
		out = next
	}
	if bytes.Equal(out, src) {
		return src, nil
	}
	if err := sameLessAggy(src, out); err != nil {
		return src, err
	}
	return out, nil
}

// stripOne deletes the first aggy hook it finds, at the widest level
// that held only it.
func stripOne(src []byte, root *node) ([]byte, bool, error) {
	if root == nil {
		return src, false, nil
	}
	hooks, err := root.member("hooks", '{')
	if err != nil || hooks == nil {
		return src, false, err
	}
	for ei, event := range hooks.vals {
		if event.kind != '[' {
			return src, false, ambiguous("hooks.%s is not a list", hooks.keys[ei])
		}
		for gi, g := range event.vals {
			if g.kind != '{' {
				return src, false, ambiguous("an entry of hooks.%s is not an object", hooks.keys[ei])
			}
			hs, err := g.member("hooks", '[')
			if err != nil {
				return src, false, ambiguous("an entry of hooks.%s has hooks that are not a list", hooks.keys[ei])
			}
			if hs == nil {
				continue
			}
			aggyCount := 0
			first := -1
			for hi, h := range hs.vals {
				if h.kind != '{' {
					return src, false, ambiguous("a hook under hooks.%s is not an object", hooks.keys[ei])
				}
				if c, _ := h.member("command", 's'); c != nil {
					if _, _, ok := aggyHook(c.str); ok {
						aggyCount++
						if first < 0 {
							first = hi
						}
					}
				}
			}
			switch {
			case aggyCount == 0:
				continue
			case aggyCount < len(hs.vals):
				return remove(src, hs, first), true, nil
			case len(event.vals) > 1:
				return remove(src, event, gi), true, nil
			case len(hooks.vals) > 1:
				return remove(src, hooks, ei), true, nil
			}
			i := root.index("hooks")
			return remove(src, root, i), true, nil
		}
	}
	return src, false, nil
}

// sameLessAggy checks that a and b mean the same once aggy's hooks are
// taken out of both, as Strip takes them out. An empty event list or
// hooks object means the same as none (D20-8): Claude's /hooks UI can
// leave "hooks": {} or "Stop": [] behind, and adding aggy's entry there
// and taking it out again must not count as a change.
func sameLessAggy(a, b []byte) error {
	va, err := lessAggy(a)
	if err != nil {
		return err
	}
	vb, err := lessAggy(b)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(pruneEmpty(va), pruneEmpty(vb)) {
		return ambiguous("the edit would change more than aggy's hooks")
	}
	return nil
}

// lessAggy decodes src and removes aggy's hooks by Strip's rules.
func lessAggy(src []byte) (any, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		return map[string]any{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, ambiguous("%v", err)
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, ambiguous("not a JSON object")
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return root, nil
	}
	removed := false
	for k, ev := range hooks {
		list, ok := ev.([]any)
		if !ok {
			continue
		}
		var keep []any
		evRemoved := false
		for _, g := range list {
			gm, ok := g.(map[string]any)
			hs, ok2 := gm["hooks"].([]any)
			if !ok || !ok2 {
				keep = append(keep, g)
				continue
			}
			var rest []any
			for _, h := range hs {
				if hm, ok := h.(map[string]any); ok {
					if c, ok := hm["command"].(string); ok {
						if _, _, ok := aggyHook(c); ok {
							continue
						}
					}
				}
				rest = append(rest, h)
			}
			switch {
			case len(rest) == len(hs):
				keep = append(keep, g)
			case len(rest) > 0:
				gm["hooks"] = rest
				keep = append(keep, g)
				evRemoved = true
			default:
				evRemoved = true
			}
		}
		if evRemoved {
			removed = true
			if len(keep) == 0 {
				delete(hooks, k)
			} else {
				hooks[k] = keep
			}
		}
	}
	if removed && len(hooks) == 0 {
		delete(root, "hooks")
	}
	return root, nil
}

// pruneEmpty drops empty event lists from a decoded file's hooks object,
// then the hooks object when that leaves it empty.
func pruneEmpty(v any) any {
	root, ok := v.(map[string]any)
	if !ok {
		return v
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return v
	}
	for k, ev := range hooks {
		if l, ok := ev.([]any); ok && len(l) == 0 {
			delete(hooks, k)
		}
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
	return root
}

// node is a parsed JSON value with its byte span in the source.
type node struct {
	kind       byte // '{', '[', 's' (string), or 'o' (number, bool, null)
	start, end int  // [start, end) of the value's text
	str        string
	keys       []string // objects
	keyStart   []int    // objects: where each member's key starts
	vals       []*node
}

func (n *node) index(k string) int {
	for i, key := range n.keys {
		if key == k {
			return i
		}
	}
	return -1
}

// member returns the value of key k in object n, which must be of kind
// (or nil when k is absent); another kind is ErrAmbiguous.
func (n *node) member(k string, kind byte) (*node, error) {
	i := n.index(k)
	if i < 0 {
		return nil, nil
	}
	if v := n.vals[i]; v.kind == kind {
		return v, nil
	}
	return nil, ambiguous("%q is not the type aggy expects", k)
}

// memberSpan returns where member or element i starts and ends.
func (n *node) memberSpan(i int) (int, int) {
	if n.kind == '{' {
		return n.keyStart[i], n.vals[i].end
	}
	return n.vals[i].start, n.vals[i].end
}

// parse reads src as one JSON object, keeping spans; empty src is nil.
func parse(src []byte) (*node, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	n, err := parseAt(dec, src, 0)
	if err != nil {
		return nil, ambiguous("%v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, ambiguous("more than one JSON value")
	}
	if n.kind != '{' {
		return nil, ambiguous("not a JSON object")
	}
	return n, nil
}

// maxDepth bounds nesting, so a hostile file cannot exhaust the stack.
const maxDepth = 200

// skip returns the offset of the next token at or after i: past white
// space and the separators , and :.
func skip(src []byte, i int) int {
	for i < len(src) && strings.IndexByte(" \t\r\n,:", src[i]) >= 0 {
		i++
	}
	return i
}

func parseAt(dec *json.Decoder, src []byte, depth int) (*node, error) {
	if depth > maxDepth {
		return nil, errors.New("nested too deeply")
	}
	start := skip(src, int(dec.InputOffset()))
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	n := &node{start: start}
	switch t {
	case json.Delim('{'):
		n.kind = '{'
		for dec.More() {
			ks := skip(src, int(dec.InputOffset()))
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k := kt.(string)
			if n.index(k) >= 0 {
				return nil, fmt.Errorf("duplicate key %q", k)
			}
			v, err := parseAt(dec, src, depth+1)
			if err != nil {
				return nil, err
			}
			n.keys, n.keyStart, n.vals = append(n.keys, k), append(n.keyStart, ks), append(n.vals, v)
		}
	case json.Delim('['):
		n.kind = '['
		for dec.More() {
			v, err := parseAt(dec, src, depth+1)
			if err != nil {
				return nil, err
			}
			n.vals = append(n.vals, v)
		}
	default:
		n.kind = 'o'
		if s, ok := t.(string); ok {
			n.kind, n.str = 's', s
		}
		n.end = int(dec.InputOffset())
		return n, nil
	}
	if _, err := dec.Token(); err != nil { // the closing delimiter
		return nil, err
	}
	n.end = int(dec.InputOffset())
	return n, nil
}

func splice(src []byte, from, to int, text string) []byte {
	out := make([]byte, 0, len(src)-(to-from)+len(text))
	out = append(out, src[:from]...)
	out = append(out, text...)
	return append(out, src[to:]...)
}

// remove deletes member or element i of container n, with one separator.
func remove(src []byte, n *node, i int) []byte {
	switch {
	case len(n.vals) == 1:
		return splice(src, n.start+1, n.end-1, "")
	case i < len(n.vals)-1:
		s, _ := n.memberSpan(i)
		next, _ := n.memberSpan(i + 1)
		return splice(src, s, next, "")
	}
	_, prevEnd := n.memberSpan(i - 1)
	_, end := n.memberSpan(i)
	return splice(src, prevEnd, end, "")
}

// insert adds a member (key, an encoded JSON string, for an object) or an
// element (key "") to container n, after its last one, in the file's
// layout: on its own line, indented like its siblings, when the
// container spans lines (or is empty in a file that has line breaks);
// inline otherwise. Line breaks match the file's: CRLF when it has any
// (D20-9).
func insert(src []byte, n *node, key string, v any) []byte {
	nl := "\n"
	if bytes.Contains(src, []byte("\r\n")) {
		nl = "\r\n"
	}
	close := n.end - 1
	multi := bytes.IndexByte(src[n.start:n.end], '\n') >= 0
	outer := lineIndent(src, n.start)
	unit := indentUnit(src)
	inner := outer + unit
	if len(n.vals) > 0 {
		s, _ := n.memberSpan(len(n.vals) - 1)
		if multi {
			inner = lineIndent(src, s)
		}
	} else if !multi {
		multi = bytes.IndexByte(bytes.TrimSpace(src), '\n') >= 0
	}
	item := encodeJSON(v, inner, unit)
	if !multi {
		item = encodeJSON(v, "", "")
	}
	item = strings.ReplaceAll(item, "\n", nl)
	if key != "" {
		item = key + ": " + item
	}
	if len(n.vals) == 0 {
		if multi {
			return splice(src, n.start+1, close, nl+inner+item+nl+outer)
		}
		return splice(src, n.start+1, close, item)
	}
	_, last := n.memberSpan(len(n.vals) - 1)
	sep := " "
	if multi {
		sep = nl + inner
	}
	return splice(src, last, last, ","+sep+item)
}

// lineIndent is the white space that starts the line holding offset i.
func lineIndent(src []byte, i int) string {
	ls := bytes.LastIndexByte(src[:i], '\n') + 1
	j := ls
	for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
		j++
	}
	return string(src[ls:j])
}

// indentUnit is the file's first line indentation, or two spaces.
func indentUnit(src []byte) string {
	for _, line := range bytes.Split(src, []byte("\n")) {
		t := bytes.TrimLeft(line, " \t")
		if len(t) > 0 && len(t) < len(line) {
			return string(line[:len(line)-len(t)])
		}
	}
	return "  "
}

// encodeJSON encodes v without HTML escaping, indented when indent is
// set, each line after the first starting with prefix.
func encodeJSON(v any, prefix, indent string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent(prefix, indent)
	_ = enc.Encode(v) // aggy's own values always encode
	return strings.TrimSuffix(b.String(), "\n")
}
