package config

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Setting names one key of the user config: Table is "" for the top level.
type Setting struct {
	Table, Key string
}

var bareKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// tableHeader matches a [table] line, and the name in it.
var tableHeader = regexp.MustCompile(`^\s*\[([^\[\]]+)\]\s*(#.*)?$`)

// SetValue returns src with s set to value, a TOML value as text, or removed
// when value is "". It edits only that line: comments and the rest stay as the
// person wrote them, and it checks the result decodes to the old document with
// only that key changed, so anything it cannot edit safely is refused.
func SetValue(src []byte, s Setting, value string) ([]byte, error) {
	if !bareKey.MatchString(s.Key) {
		return nil, fmt.Errorf("config: %q is not a key aggy edits", s.Key)
	}
	old := map[string]any{}
	if err := toml.Unmarshal(src, &old); err != nil {
		return nil, err
	}
	nl := "\n"
	if strings.Contains(string(src), "\r\n") {
		nl = "\r\n"
	}
	lines := strings.SplitAfter(string(src), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	keyLine := regexp.MustCompile(`^\s*"?` + regexp.QuoteMeta(s.Key) + `"?\s*=`)
	current, tableAt, firstTable, keyAt := "", -1, -1, -1
	for i, l := range lines {
		if m := tableHeader.FindStringSubmatch(strings.TrimRight(l, "\r\n")); m != nil {
			current = strings.TrimSpace(m[1])
			if firstTable < 0 {
				firstTable = i
			}
			if s.Table != "" && current == s.Table {
				tableAt = i
			}
			continue
		}
		if current == s.Table && keyLine.MatchString(l) {
			keyAt = i
		}
	}
	text := s.Key + " = " + value + nl
	switch {
	case keyAt >= 0 && value == "":
		lines = append(lines[:keyAt:keyAt], lines[keyAt+1:]...)
	case keyAt >= 0:
		lines[keyAt] = text
	case value == "":
		return src, nil
	case s.Table == "" && firstTable >= 0:
		lines = append(lines[:firstTable:firstTable], append([]string{text, nl}, lines[firstTable:]...)...)
	case s.Table == "":
		lines = append(lines, text)
	case tableAt >= 0:
		lines = append(lines[:tableAt+1:tableAt+1], append([]string{text}, lines[tableAt+1:]...)...)
	default:
		if n := len(lines); n > 0 {
			if !strings.HasSuffix(lines[n-1], "\n") {
				lines[n-1] += nl
			}
			lines = append(lines, nl)
		}
		lines = append(lines, "["+s.Table+"]"+nl, text)
	}
	out := []byte(strings.Join(lines, ""))
	got := map[string]any{}
	if err := toml.Unmarshal(out, &got); err != nil {
		return nil, fmt.Errorf("config: this edit would break the file: %w", err)
	}
	if err := sameExcept(old, got, s, value); err != nil {
		return nil, err
	}
	return out, nil
}

// sameExcept checks the two documents differ only at s.
func sameExcept(old, got map[string]any, s Setting, value string) error {
	path := []string{}
	if s.Table != "" {
		path = strings.Split(s.Table, ".")
	}
	strip := func(doc map[string]any) (any, bool) {
		cur := doc
		for _, p := range path {
			next, ok := cur[p].(map[string]any)
			if !ok {
				return nil, false
			}
			cur = next
		}
		v, ok := cur[s.Key]
		delete(cur, s.Key)
		return v, ok
	}
	_, ok := strip(got)
	if value != "" && !ok {
		return errors.New("config: aggy could not make this edit, so the file is unchanged; change it by hand")
	}
	strip(old)
	if !reflect.DeepEqual(prune(old), prune(got)) {
		return errors.New("config: this edit would change something else in the file, so it is unchanged; change it by hand")
	}
	return nil
}

// prune drops empty tables, which adding a table's first key leaves behind.
func prune(doc map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range doc {
		if t, ok := v.(map[string]any); ok {
			if p := prune(t); len(p) > 0 {
				out[k] = p
			}
			continue
		}
		out[k] = v
	}
	return out
}

// Quote is a string as a TOML value.
func Quote(s string) string { return strconv.Quote(s) }

// LineError is a mistake in a hand-edited file. Line is 0 when it has no line.
type LineError struct {
	Line int
	Msg  string
}

// CheckFile reports what is wrong with config text, by line where it can.
func CheckFile(text string) []LineError {
	cfg := Default()
	md, err := toml.Decode(text, &cfg)
	if err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			return []LineError{{pe.Position.Line, pe.Message}}
		}
		return []LineError{{0, err.Error()}}
	}
	var out []LineError
	for _, k := range md.Undecoded() {
		key := k[len(k)-1]
		out = append(out, LineError{lineOfKey(text, key), fmt.Sprintf("aggy does not know the key %s", k.String())})
	}
	if err := cfg.Validate(); err != nil {
		out = append(out, LineError{0, err.Error()})
	}
	return out
}

func lineOfKey(text, key string) int {
	re := regexp.MustCompile(`^\s*\[*"?` + regexp.QuoteMeta(key) + `"?\]*\s*(=|$)`)
	for i, l := range strings.Split(text, "\n") {
		if re.MatchString(strings.TrimRight(l, "\r")) {
			return i + 1
		}
	}
	return 0
}
