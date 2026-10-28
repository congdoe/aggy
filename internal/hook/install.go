package hook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode"

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

// Command is the shell command a hook config runs for e: aggy's absolute
// path, single-quoted, then "hook --vendor <vendor> <event>". Claude runs
// a command without args through sh -c (hooks.md), and Grok takes "an
// inline shell command" (10-hooks.md); the payload goes to stdin, never
// into the command. bin must be a clean absolute path with no quote or
// control character.
func Command(bin string, e Entry) (string, error) {
	if err := CheckBin(bin); err != nil {
		return "", err
	}
	return "'" + bin + "' hook --vendor " + string(e.Vendor) + " " + e.Event, nil
}

// CheckBin refuses a path aggy cannot put in a hook config as is.
func CheckBin(bin string) error {
	if !filepath.IsAbs(bin) || filepath.Clean(bin) != bin {
		return fmt.Errorf("aggy's path %q is not a clean absolute path", bin)
	}
	if strings.ContainsFunc(bin, func(r rune) bool { return r == '\'' || unicode.IsControl(r) }) {
		return fmt.Errorf("aggy's path %q holds a quote or control character, which a hook command cannot carry", bin)
	}
	return nil
}

// ErrAmbiguous means aggy will not merge its hooks into a file, because
// the result would not be clearly the file plus aggy's entries.
var ErrAmbiguous = errors.New("ambiguous hook config")

// ambiguous wraps a reason in ErrAmbiguous.
func ambiguous(format string, a ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrAmbiguous}, a...)...)
}

// Missing returns the entries of want that the Claude-format hooks file
// src ({"hooks": {"<Event>": [{"matcher": ..., "hooks": [{"type":
// "command", "command": ...}]}]}}) does not hold yet. nil or empty src
// holds none. A file aggy cannot read unambiguously is an error
// (ErrAmbiguous): not a JSON object, a duplicate key, "hooks" or an event
// of the wrong type, or an aggy hook of the same vendor and event that
// calls another aggy binary.
func Missing(src []byte, bin string, want []Entry) ([]Entry, error) {
	root, err := parseRoot(src)
	if err != nil {
		return nil, err
	}
	hooks, err := root.object("hooks")
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range want {
		cmd, err := Command(bin, e)
		if err != nil {
			return nil, err
		}
		found := false
		var groups []any
		if hooks != nil {
			v, ok := hooks.get(e.Event)
			if ok {
				if groups, ok = v.([]any); !ok {
					return nil, ambiguous("hooks.%s is not a list", e.Event)
				}
			}
		}
		for _, g := range groups {
			cmds, err := groupCommands(g, e.Event)
			if err != nil {
				return nil, err
			}
			for _, c := range cmds {
				switch {
				case c == cmd:
					found = true
				case isAggyHook(c, e):
					return nil, ambiguous("hooks.%s already runs aggy from another path (%s); remove that entry or run aggy unwire first", e.Event, c)
				}
			}
		}
		if !found {
			out = append(out, e)
		}
	}
	return out, nil
}

// groupCommands returns the command strings of one matcher group.
func groupCommands(g any, event string) ([]string, error) {
	o, ok := g.(*object)
	if !ok {
		return nil, ambiguous("an entry of hooks.%s is not an object", event)
	}
	v, ok := o.get("hooks")
	if !ok {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, ambiguous("an entry of hooks.%s has hooks that are not a list", event)
	}
	var out []string
	for _, h := range list {
		ho, ok := h.(*object)
		if !ok {
			return nil, ambiguous("a hook under hooks.%s is not an object", event)
		}
		if c, ok := ho.get("command"); ok {
			if s, ok := c.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// isAggyHook reports whether cmd is an aggy hook command for e, with
// any binary path: "<path>/aggy hook --vendor <v> <event>", the path
// quoted or not.
func isAggyHook(cmd string, e Entry) bool {
	prog, ok := strings.CutSuffix(cmd, " hook --vendor "+string(e.Vendor)+" "+e.Event)
	if !ok {
		return false
	}
	prog = strings.Trim(prog, `'"`)
	return filepath.Base(prog) == "aggy"
}

// Merge returns a patch transform that adds the entries Missing reports
// to a Claude-format hooks file, each as its own matcher group (no
// matcher: every occurrence) with one command hook. Existing keys keep
// their order and values; numbers keep their spelling. When nothing is
// missing it returns src unchanged, so it is idempotent.
func Merge(bin string, want []Entry) func([]byte) ([]byte, error) {
	return func(src []byte) ([]byte, error) {
		missing, err := Missing(src, bin, want)
		if err != nil || len(missing) == 0 {
			return src, err
		}
		root, _ := parseRoot(src)
		hooks, _ := root.object("hooks")
		if hooks == nil {
			hooks = &object{}
			root.set("hooks", hooks)
		}
		for _, e := range missing {
			cmd, _ := Command(bin, e)
			h := &object{}
			h.set("type", "command")
			h.set("command", cmd)
			h.set("timeout", json.Number(fmt.Sprint(Timeout)))
			g := &object{}
			g.set("hooks", []any{h})
			v, _ := hooks.get(e.Event)
			list, _ := v.([]any)
			hooks.set(e.Event, append(list, g))
		}
		var b bytes.Buffer
		encode(&b, root, "")
		b.WriteByte('\n')
		return b.Bytes(), nil
	}
}

// object is a JSON object that keeps its key order.
type object struct {
	keys []string
	vals []any
}

func (o *object) get(k string) (any, bool) {
	for i, key := range o.keys {
		if key == k {
			return o.vals[i], true
		}
	}
	return nil, false
}

func (o *object) set(k string, v any) {
	for i, key := range o.keys {
		if key == k {
			o.vals[i] = v
			return
		}
	}
	o.keys = append(o.keys, k)
	o.vals = append(o.vals, v)
}

// object returns the object at key k, nil when k is absent.
func (o *object) object(k string) (*object, error) {
	v, ok := o.get(k)
	if !ok {
		return nil, nil
	}
	c, ok := v.(*object)
	if !ok {
		return nil, ambiguous("%q is not an object", k)
	}
	return c, nil
}

// parseRoot parses src as one JSON object; empty src is {}.
func parseRoot(src []byte) (*object, error) {
	if len(bytes.TrimSpace(src)) == 0 {
		return &object{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	v, err := decode(dec)
	if err != nil {
		return nil, ambiguous("%v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, ambiguous("more than one JSON value")
	}
	o, ok := v.(*object)
	if !ok {
		return nil, ambiguous("not a JSON object")
	}
	return o, nil
}

// maxDepth bounds nesting, so a hostile file cannot exhaust the stack.
const maxDepth = 200

func decode(dec *json.Decoder) (any, error) { return decodeAt(dec, 0) }

func decodeAt(dec *json.Decoder, depth int) (any, error) {
	if depth > maxDepth {
		return nil, errors.New("nested too deeply")
	}
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t {
	case json.Delim('{'):
		o := &object{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k := kt.(string)
			if _, dup := o.get(k); dup {
				return nil, fmt.Errorf("duplicate key %q", k)
			}
			v, err := decodeAt(dec, depth+1)
			if err != nil {
				return nil, err
			}
			o.keys, o.vals = append(o.keys, k), append(o.vals, v)
		}
		_, err := dec.Token()
		return o, err
	case json.Delim('['):
		list := []any{}
		for dec.More() {
			v, err := decodeAt(dec, depth+1)
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		_, err := dec.Token()
		return list, err
	}
	return t, nil
}

// encode writes v indented by two spaces, as Claude Code writes its
// settings, without HTML escaping.
func encode(b *bytes.Buffer, v any, indent string) {
	switch x := v.(type) {
	case *object:
		if len(x.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range x.keys {
			b.WriteString(indent + "  ")
			encodeScalar(b, k)
			b.WriteString(": ")
			encode(b, x.vals[i], indent+"  ")
			if i < len(x.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "}")
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range x {
			b.WriteString(indent + "  ")
			encode(b, e, indent+"  ")
			if i < len(x)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(indent + "]")
	default:
		encodeScalar(b, x)
	}
}

func encodeScalar(b *bytes.Buffer, v any) {
	var s bytes.Buffer
	enc := json.NewEncoder(&s)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v) // strings, json.Number, bools, and nil always encode
	b.Write(bytes.TrimSuffix(s.Bytes(), []byte("\n")))
}
