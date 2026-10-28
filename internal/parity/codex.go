package parity

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/BurntSushi/toml"

	"github.com/congdoe/aggy/internal/patch"
)

// FallbackKey is the Codex config key listing the instruction files Codex
// reads when a directory has no AGENTS.md. Codex takes it as a top-level
// array of file names in config.toml, for example
// project_doc_fallback_filenames = ["CLAUDE.md"].
const FallbackKey = "project_doc_fallback_filenames"

// FallbackName is the file aggy adds to FallbackKey, so Codex reads a
// repo's CLAUDE.md when it has no AGENTS.md.
const FallbackName = "CLAUDE.md"

// CodexConfigPath returns Codex's config file: $CODEX_HOME/config.toml
// when CODEX_HOME is set, else ~/.codex/config.toml. A relative
// CODEX_HOME is an error, as it is to safefs.
func CodexConfigPath() (string, error) {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("parity: CODEX_HOME=%q is not an absolute path", d)
		}
		return filepath.Join(d, "config.toml"), nil
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return "", errors.New("parity: cannot find the home directory")
	}
	return filepath.Join(h, ".codex", "config.toml"), nil
}

// CodexFallbackPatch returns the patch that adds CLAUDE.md to
// project_doc_fallback_filenames in the Codex config at path, guarded by
// the file's current hash. Apply it through a manifest, so aggy unwire
// can undo it.
//
// The change is a minimal text edit, never a re-serialization, so
// comments, layout, and line endings (CRLF included) are kept:
//
//   - no file: it is created, mode 0600, holding only the key
//   - no key: one line, `project_doc_fallback_filenames = ["CLAUDE.md"]`,
//     goes in at the top level, after a leading comment block that ends
//     in a blank line, or else first in the file
//   - a list without CLAUDE.md: "CLAUDE.md" is appended inside its
//     brackets, after the last entry, and the other entries are kept
//   - a list with CLAUDE.md: nothing changes
//   - a leading UTF-8 byte order mark is kept in front, in every case
//
// The result is decoded and compared with the original: every other
// value must be unchanged and the key must be the old list plus
// CLAUDE.md, or the patch is refused. A file that is not valid TOML, or
// whose key is not a list of strings, is refused too.
func CodexFallbackPatch(path string) (patch.Patch, error) {
	h, err := patch.HashFile(path)
	if err != nil {
		return patch.Patch{}, err
	}
	return patch.Patch{File: path, ExpectedHash: h, Transform: AddClaudeFallback, Mode: 0o600}, nil
}

// AddClaudeFallback is CodexFallbackPatch's transform. It gets nil for a
// file that does not exist.
func AddClaudeFallback(src []byte) ([]byte, error) {
	line := FallbackKey + ` = ["` + FallbackName + `"]`
	if src == nil {
		return []byte(line + "\n"), nil
	}
	var doc map[string]any
	if _, err := toml.Decode(string(src), &doc); err != nil {
		return nil, fmt.Errorf("the Codex config is not valid TOML, so aggy leaves it alone: %w", err)
	}
	var list []string
	cur, present := doc[FallbackKey]
	if present {
		var ok bool
		if list, ok = stringList(cur); !ok {
			return nil, fmt.Errorf("%s is not a list of file names", FallbackKey)
		}
		if slices.Contains(list, FallbackName) {
			return src, nil
		}
	}
	var out []byte
	var err error
	if present {
		out, err = appendToArray(src)
	} else {
		out = insertLine(src, line)
	}
	if err != nil {
		return nil, err
	}
	// Prove the edit: the result decodes to the old document with only
	// the list extended.
	var after map[string]any
	if _, err := toml.Decode(string(out), &after); err != nil {
		return nil, fmt.Errorf("cannot add %s to %s safely: %w", FallbackName, FallbackKey, err)
	}
	want := make([]any, 0, len(list)+1)
	for _, s := range list {
		want = append(want, s)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	doc[FallbackKey] = append(want, FallbackName)
	if !reflect.DeepEqual(after, doc) {
		return nil, fmt.Errorf("cannot add %s to %s safely: the edit would change other settings", FallbackName, FallbackKey)
	}
	return out, nil
}

// stringList returns v as a list of strings.
func stringList(v any) ([]string, bool) {
	vs, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(vs))
	for _, x := range vs {
		s, ok := x.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// eol returns the file's line ending: CRLF when it has any, else LF.
func eol(src []byte) string {
	if bytes.Contains(src, []byte("\r\n")) {
		return "\r\n"
	}
	return "\n"
}

var bom = []byte("\xef\xbb\xbf")

// insertLine puts line at the top level: after a leading comment block
// that ends in a blank line (a file header), or else first, after a
// byte order mark if there is one. Anything before the first table
// header is top level, so the result is valid wherever the header is.
func insertLine(src []byte, line string) []byte {
	nl := eol(src)
	at := 0
	if bytes.HasPrefix(src, bom) {
		at = len(bom)
	}
	// Walk the leading comment lines; a blank line after them ends the
	// header block.
	i := at
	sawComment := false
	for i < len(src) {
		end := bytes.IndexByte(src[i:], '\n')
		if end < 0 {
			break
		}
		l := bytes.TrimSpace(src[i : i+end])
		next := i + end + 1
		if len(l) > 0 && l[0] == '#' {
			sawComment = true
			i = next
			continue
		}
		if len(l) == 0 && sawComment {
			at = next
		}
		break
	}
	out := make([]byte, 0, len(src)+len(line)+2)
	out = append(out, src[:at]...)
	out = append(out, line...)
	out = append(out, nl...)
	return append(out, src[at:]...)
}

// appendToArray adds "CLAUDE.md" as the last entry of the top-level
// FallbackKey array.
func appendToArray(src []byte) ([]byte, error) {
	open, err := findArray(src)
	if err != nil {
		return nil, err
	}
	at, text, err := arrayInsert(src, open)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(src)+len(text))
	out = append(out, src[:at]...)
	out = append(out, text...)
	return append(out, src[at:]...), nil
}

var errNotFound = fmt.Errorf("cannot find the top-level %s line to edit", FallbackKey)

// findArray returns the index of the '[' that opens the FallbackKey
// array at the top level, before any table header. It skips comments,
// strings, and the insides of arrays and inline tables, so a key name in
// a comment or a string, or a key of a table, does not match. A key
// written quoted is not found, and the patch is refused.
func findArray(src []byte) (int, error) {
	depth := 0
	lineStart := true
	// A UTF-8 byte order mark is not part of the first line's key.
	start := 0
	if bytes.HasPrefix(src, bom) {
		start = len(bom)
	}
	for i := start; i < len(src); {
		c := src[i]
		switch {
		case c == '\n':
			lineStart = true
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '#':
			i = skipComment(src, i)
		case c == '"' || c == '\'':
			end, err := skipString(src, i)
			if err != nil {
				return 0, err
			}
			i, lineStart = end, false
		case c == '[' && depth == 0 && lineStart:
			return 0, errNotFound // a table header: the top level is over
		case c == '[' || c == '{':
			depth++
			i, lineStart = i+1, false
		case c == ']' || c == '}':
			depth--
			i, lineStart = i+1, false
		case lineStart && depth == 0 && bytes.HasPrefix(src[i:], []byte(FallbackKey)):
			j := i + len(FallbackKey)
			for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
				j++
			}
			if j < len(src) && src[j] == '=' {
				j++
				for j < len(src) && (src[j] == ' ' || src[j] == '\t') {
					j++
				}
				if j < len(src) && src[j] == '[' {
					return j, nil
				}
				return 0, errNotFound
			}
			i, lineStart = i+1, false
		default:
			i, lineStart = i+1, false
		}
	}
	return 0, errNotFound
}

// arrayInsert scans the array opening at src[open] and returns where to
// insert the new entry and the text to insert: right after '[' in an
// empty array, after a trailing comma, or after the last entry with a
// comma in front.
func arrayInsert(src []byte, open int) (int, string, error) {
	const entry = `"` + FallbackName + `"`
	depth := 1
	lastEnd, comma := -1, -1
	for i := open + 1; i < len(src); {
		switch src[i] {
		case ' ', '\t', '\r', '\n':
			i++
		case '#':
			i = skipComment(src, i)
		case '"', '\'':
			end, err := skipString(src, i)
			if err != nil {
				return 0, "", err
			}
			if depth == 1 {
				lastEnd, comma = end, -1
			}
			i = end
		case '[', '{':
			depth++
			i++
		case ']', '}':
			depth--
			i++
			if depth == 0 {
				switch {
				case lastEnd < 0:
					return open + 1, entry, nil
				case comma >= 0:
					return comma, " " + entry + ",", nil
				default:
					return lastEnd, ", " + entry, nil
				}
			}
			if depth == 1 {
				lastEnd, comma = i, -1
			}
		case ',':
			if depth == 1 {
				comma = i + 1
			}
			i++
		default:
			i++
			if depth == 1 {
				lastEnd, comma = i, -1
			}
		}
	}
	return 0, "", fmt.Errorf("the %s array is not closed", FallbackKey)
}

// skipComment returns the index of the newline ending the comment at i.
func skipComment(src []byte, i int) int {
	if n := bytes.IndexByte(src[i:], '\n'); n >= 0 {
		return i + n
	}
	return len(src)
}

// skipString returns the index just past the TOML string starting at
// src[i]: basic or literal, single or multi-line.
func skipString(src []byte, i int) (int, error) {
	q := src[i]
	if bytes.HasPrefix(src[i:], []byte{q, q, q}) {
		delim := []byte{q, q, q}
		j := i + 3
		for j < len(src) {
			if q == '"' && src[j] == '\\' {
				j += 2
				continue
			}
			if bytes.HasPrefix(src[j:], delim) {
				j += 3
				// Up to two more quotes belong to the content.
				for k := 0; k < 2 && j < len(src) && src[j] == q; k++ {
					j++
				}
				return j, nil
			}
			j++
		}
		return 0, errors.New("unclosed multi-line string")
	}
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			if q == '"' {
				j++
			}
		case q:
			return j + 1, nil
		case '\n':
			return 0, errors.New("unclosed string")
		}
	}
	return 0, errors.New("unclosed string")
}
