// Package claudetrust records, and takes back, Claude Code's own trust for one
// folder (T6977), so Claude's trust dialog does not appear where the person
// already answered aggy's trust list.
//
// Claude keeps a folder's trust in ~/.claude.json, or in
// $CLAUDE_CONFIG_DIR/.claude.json when that is set, under
// projects["<absolute folder>"].hasTrustDialogAccepted. Live check,
// 2026-11-10, Claude Code 2.1.322 on macOS (docs/verification.md): with that
// key written true in a throwaway config folder Claude skipped its trust
// screen; with false it showed it; cancelling the screen recorded nothing.
// Not checked: a signed-in run with the key written ahead, whether a trusted
// parent folder covers a child, and how Claude spells the folder (aggy writes
// the real path).
//
// The file also holds Claude's history and account state, so this package
// reads it only to find that one key, never returns or logs anything else from
// it, and changes it by a minimal text edit: the rest stays byte for byte, and
// the result is checked to decode to the old document with that one change.
// A change through here takes no backup of the file (patch.Patch.NoBackup):
// aggy unwire puts back the one key (Undo) instead of the old bytes, so what
// Claude wrote since is kept.
package claudetrust

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
)

// Key is Claude's per-folder trust key.
const Key = "hasTrustDialogAccepted"

// FileName is the file in the home folder, or in CLAUDE_CONFIG_DIR, that holds
// it.
const FileName = ".claude.json"

// MaxFile caps the file: it also holds Claude's history of projects.
const MaxFile = 32 << 20

// ReversePrefix starts the manifest's Reverse for a change made here:
// ReversePrefix + before + ":" + folder.
const ReversePrefix = "claude-trust:"

// Before is what the folder's entry held before aggy set the key, which an
// undo puts back.
type Before string

// The states Before names.
const (
	// WasFalse: the key was there, false.
	WasFalse Before = "false"
	// NoKey: the folder's entry was there without the key.
	NoKey Before = "nokey"
	// NoEntry: projects was there without the folder.
	NoEntry Before = "noentry"
	// NoProjects: the file had no projects at all.
	NoProjects Before = "noprojects"
	// WasTrue: the key was true already, so nothing changes.
	WasTrue Before = "true"
)

// Path is the file Claude keeps a folder's trust in: $CLAUDE_CONFIG_DIR/.claude.json
// when that is set (the live check above), else ~/.claude.json. Either must
// be absolute.
func Path(getenv func(string) string) (string, error) {
	if d := getenv("CLAUDE_CONFIG_DIR"); d != "" {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("CLAUDE_CONFIG_DIR=%q is not an absolute path", d)
		}
		return filepath.Join(d, FileName), nil
	}
	h := getenv("HOME")
	if !filepath.IsAbs(h) {
		return "", errors.New("cannot find the home directory")
	}
	return filepath.Join(h, FileName), nil
}

// State reads what src says of folder's trust.
func State(src []byte, folder string) (Before, error) {
	if err := checkFolder(folder); err != nil {
		return "", err
	}
	f, err := locate(src, path(folder))
	if err != nil {
		return "", err
	}
	switch len(f.mems) {
	case 0:
		return NoProjects, nil
	case 1:
		if !f.mems[0].obj {
			return "", errors.New("its projects is not an object")
		}
		return NoEntry, nil
	case 2:
		if !f.mems[1].obj {
			return "", errors.New("its entry for this folder is not an object")
		}
		return NoKey, nil
	}
	switch f.mems[2].tok {
	case true:
		return WasTrue, nil
	case false:
		return WasFalse, nil
	}
	return "", fmt.Errorf("its %s for this folder is neither true nor false", Key)
}

// Set returns src with folder's key set true: a minimal text edit, checked to
// decode to src's document with that one change. src already trusting the
// folder comes back unchanged.
func Set(src []byte, folder string) ([]byte, error) {
	before, err := State(src, folder)
	if err != nil {
		return nil, err
	}
	if before == WasTrue {
		return src, nil
	}
	f, err := locate(src, path(folder))
	if err != nil {
		return nil, err
	}
	var out []byte
	switch before {
	case WasFalse:
		v := f.mems[2].val
		out = splice(src, v.start, v.end, "true")
	case NoKey:
		out = insert(src, f.objs[2], quote(Key)+": true")
	case NoEntry:
		out = insert(src, f.objs[1], quote(folder)+": {"+quote(Key)+": true}")
	case NoProjects:
		out = insert(src, f.objs[0], quote("projects")+": {"+quote(folder)+": {"+quote(Key)+": true}}")
	}
	if err := sameBut(src, out, func(doc map[string]any) {
		projects, ok := doc["projects"].(map[string]any)
		if !ok {
			projects = map[string]any{}
			doc["projects"] = projects
		}
		entry, ok := projects[folder].(map[string]any)
		if !ok {
			entry = map[string]any{}
			projects[folder] = entry
		}
		entry[Key] = true
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// Undo returns src with folder's key put back as before says it was, leaving
// everything else as it is now: an entry aggy made is removed only while it
// holds nothing else, and so is a projects aggy made. A key no longer true
// (Claude, or the person, changed it since) is left alone.
func Undo(src []byte, folder string, before Before) ([]byte, error) {
	if err := checkFolder(folder); err != nil {
		return nil, err
	}
	now, err := State(src, folder)
	if err != nil {
		return nil, err
	}
	if now != WasTrue || before == WasTrue {
		return src, nil
	}
	out := src
	f, err := locate(out, path(folder))
	if err != nil {
		return nil, err
	}
	if before == WasFalse {
		v := f.mems[2].val
		out = splice(out, v.start, v.end, "false")
	} else {
		out = remove(out, f.objs[2], f.mems[2])
		for depth := 1; depth >= 0; depth-- {
			if (depth == 1 && before == NoKey) || (depth == 0 && before != NoProjects) {
				break
			}
			if f, err = locate(out, path(folder)); err != nil {
				return nil, err
			}
			if f.objs[depth+1].n != 0 {
				break // it holds something else now: kept
			}
			out = remove(out, f.objs[depth], f.mems[depth])
		}
	}
	if err := sameBut(src, out, func(doc map[string]any) {
		projects := doc["projects"].(map[string]any)
		entry := projects[folder].(map[string]any)
		if before == WasFalse {
			entry[Key] = false
			return
		}
		delete(entry, Key)
		if before == NoKey || len(entry) > 0 {
			return
		}
		delete(projects, folder)
		if before == NoProjects && len(projects) == 0 {
			delete(doc, "projects")
		}
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// Reverse is the manifest's Reverse for setting folder's key from before.
func Reverse(before Before, folder string) string {
	return ReversePrefix + string(before) + ":" + folder
}

// ParseReverse reads a Reverse made by Reverse.
func ParseReverse(rev string) (Before, string, bool) {
	rest, ok := strings.CutPrefix(rev, ReversePrefix)
	if !ok {
		return "", "", false
	}
	b, folder, ok := strings.Cut(rest, ":")
	switch Before(b) {
	case WasFalse, NoKey, NoEntry, NoProjects:
	default:
		return "", "", false
	}
	if !ok || checkFolder(folder) != nil {
		return "", "", false
	}
	return Before(b), folder, true
}

// checkFolder refuses a folder that cannot be a key aggy writes.
func checkFolder(folder string) error {
	if !filepath.IsAbs(folder) || strings.ContainsAny(folder, "\n\r\x00") {
		return errors.New("claudetrust: the folder must be an absolute path on one line")
	}
	return nil
}

func path(folder string) []string { return []string{"projects", folder, Key} }

// quote is s as a JSON string, with no HTML escaping.
func quote(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// span is [start, end) in the source.
type span struct{ start, end int }

// object is an object on the path: where its { is, and how many members it has.
type object struct {
	open int
	n    int
}

// member is a member on the path: its key's start, its value, and, for a
// scalar, the value; obj when the value is an object.
type member struct {
	key int
	val span
	tok json.Token
	obj bool
}

// found is what locate found: objs[i] holds the member path[i], and mems[i]
// is that member; mems stops at the first one missing, and objs at the first
// member that is no object.
type found struct {
	objs []object
	mems []member
}

// locate walks src, one JSON object, and finds path in it. A key on the path
// that appears twice in its object is refused: which one Claude reads is not
// aggy's to guess.
func locate(src []byte, p []string) (found, error) {
	var f found
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	tok, start, _, err := next(dec, src)
	if err != nil {
		return f, badJSON(err)
	}
	if tok != json.Delim('{') {
		return f, errors.New("it is not a JSON object")
	}
	if err := walk(dec, src, start, p, 0, &f); err != nil {
		return f, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return f, errors.New("it is not valid JSON: something follows the object")
	}
	return f, nil
}

// walk reads one object whose { is at open, on the path at depth.
func walk(dec *json.Decoder, src []byte, open int, p []string, depth int, f *found) error {
	f.objs = append(f.objs, object{open: open})
	at := len(f.objs) - 1
	seen := false
	for {
		tok, start, _, err := next(dec, src)
		if err != nil {
			return badJSON(err)
		}
		if tok == json.Delim('}') {
			return nil
		}
		key, ok := tok.(string)
		if !ok {
			return errors.New("it is not valid JSON")
		}
		f.objs[at].n++
		vt, vs, ve, err := next(dec, src)
		if err != nil {
			return badJSON(err)
		}
		match := depth < len(p) && key == p[depth]
		if match && seen {
			return fmt.Errorf("it names %q twice in one object", key)
		}
		if !match {
			if err := skip(dec, vt); err != nil {
				return err
			}
			continue
		}
		seen = true
		m := member{key: start, val: span{vs, ve}, tok: vt, obj: vt == json.Delim('{')}
		f.mems = append(f.mems, m)
		mi := len(f.mems) - 1
		if m.obj && depth+1 < len(p) {
			if err := walk(dec, src, vs, p, depth+1, f); err != nil {
				return err
			}
		} else if err := skip(dec, vt); err != nil {
			return err
		}
		f.mems[mi].val.end = int(dec.InputOffset())
	}
}

// skip reads the rest of a value whose first token was tok.
func skip(dec *json.Decoder, tok json.Token) error {
	d, ok := tok.(json.Delim)
	if !ok || d == '}' || d == ']' {
		return nil
	}
	for depth := 1; depth > 0; {
		t, err := dec.Token()
		if err != nil {
			return badJSON(err)
		}
		switch t {
		case json.Delim('{'), json.Delim('['):
			depth++
		case json.Delim('}'), json.Delim(']'):
			depth--
		}
	}
	return nil
}

// next reads a token and where it is: the decoder's offset before it can sit
// on the separators and space before the token, which are skipped.
func next(dec *json.Decoder, src []byte) (json.Token, int, int, error) {
	pre := int(dec.InputOffset())
	tok, err := dec.Token()
	if err != nil {
		return nil, 0, 0, err
	}
	end := int(dec.InputOffset())
	start := pre
	for start < end && strings.IndexByte(" \t\r\n,:", src[start]) >= 0 {
		start++
	}
	return tok, start, end, nil
}

func splice(src []byte, start, end int, text string) []byte {
	out := append([]byte{}, src[:start]...)
	out = append(out, text...)
	return append(out, src[end:]...)
}

// insert puts member text first in the object o.
func insert(src []byte, o object, member string) []byte {
	if o.n > 0 {
		member += ", "
	}
	return splice(src, o.open+1, o.open+1, member)
}

// remove takes member m out of object o, with the comma that joins it to
// another member.
func remove(src []byte, o object, m member) []byte {
	end := m.val.end
	i := end
	for i < len(src) && isSpace(src[i]) {
		i++
	}
	if i < len(src) && src[i] == ',' {
		i++
		for i < len(src) && isSpace(src[i]) {
			i++
		}
		return splice(src, m.key, i, "")
	}
	start := m.key
	j := start - 1
	for j > o.open && isSpace(src[j]) {
		j--
	}
	if j > o.open && src[j] == ',' {
		start = j
	}
	return splice(src, start, end, "")
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// sameBut checks out decodes to src's document changed by change, and to
// nothing else.
func sameBut(src, out []byte, change func(map[string]any)) error {
	want, err := decode(src)
	if err != nil {
		return err
	}
	change(want)
	got, err := decode(out)
	if err != nil {
		return fmt.Errorf("claudetrust: the edit would break the file: %w", err)
	}
	if !reflect.DeepEqual(got, want) {
		return errors.New("claudetrust: the edit would change something else in the file")
	}
	return nil
}

// badJSON says the file is not valid JSON, and where, without the decoder's
// words, which can quote a character of the file.
func badJSON(err error) error {
	var se *json.SyntaxError
	if errors.As(err, &se) {
		return fmt.Errorf("it is not valid JSON (at byte %d)", se.Offset)
	}
	return errors.New("it is not valid JSON")
}

func decode(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, badJSON(err)
	}
	if doc == nil {
		return nil, errors.New("it is not a JSON object")
	}
	return doc, nil
}
