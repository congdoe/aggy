package permissions

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

func (o *Object) list(k Kind) []string {
	switch k {
	case Allow:
		return o.Allow
	case Ask:
		return o.Ask
	}
	return o.Deny
}

func (r *RepoRules) list(k Kind) []string {
	switch k {
	case Allow:
		return r.Allow
	case Ask:
		return r.Ask
	}
	return r.Deny
}

// matchingKey finds the key the file holds for a repo: it is edited by hand, so
// it may be spelled another way than the canonical path.
func matchingKey(keys []string, repoKey string) string {
	slices.Sort(keys)
	for _, k := range keys {
		if k == repoKey || canonical(k) == repoKey {
			return k
		}
	}
	return repoKey
}

// Edit is one change to aggy's own file. RepoKey "" means every repo.
type Edit struct {
	Kind    Kind
	Rule    string
	RepoKey string
	Remove  bool
}

// Apply changes the file's text, with the rule in or out. It is deterministic,
// and applying it to its own output returns that output, as internal/patch
// needs. It edits the document as it stands, so a key aggy does not use, such as
// $schema, stays; the file has no comments, so only spacing can change.
func (e Edit) Apply(src []byte) ([]byte, error) {
	doc := map[string]any{}
	if len(src) > 0 {
		if _, err := ParseAggyFile(src); err != nil {
			return nil, fmt.Errorf("%s is not valid: %w; aggy doctor says where", FileName, err)
		}
		if err := json.Unmarshal(src, &doc); err != nil {
			return nil, err
		}
	}
	section := func(m map[string]any, key string) map[string]any {
		if sub, ok := m[key].(map[string]any); ok {
			return sub
		}
		sub := map[string]any{}
		m[key] = sub
		return sub
	}
	var object map[string]any
	var repos map[string]any
	key := e.RepoKey
	if key == "" {
		object = section(doc, "permissions")
	} else {
		repos = section(doc, "repos")
		key = matchingKey(slices.Collect(maps.Keys(repos)), key)
		object = section(repos, key)
	}
	var list []any
	if l, ok := object[string(e.Kind)].([]any); ok {
		list = l
	}
	i := slices.IndexFunc(list, func(v any) bool { return v == e.Rule })
	switch {
	case e.Remove && i >= 0:
		list = slices.Delete(list, i, i+1)
	case !e.Remove && i < 0:
		list = append(list, e.Rule)
	default:
		return src, nil
	}
	if len(list) == 0 {
		delete(object, string(e.Kind))
	} else {
		object[string(e.Kind)] = list
	}
	if key != "" && len(object) == 0 {
		delete(repos, key)
	}
	if key != "" && len(repos) == 0 {
		delete(doc, "repos")
	}
	if p, ok := doc["permissions"].(map[string]any); ok && len(p) == 0 {
		delete(doc, "permissions")
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// Has reports whether the file holds the rule at that scope.
func Has(src []byte, k Kind, rule, repoKey string) (bool, error) {
	if len(src) == 0 {
		return false, nil
	}
	f, err := ParseAggyFile(src)
	if err != nil {
		return false, err
	}
	if repoKey == "" {
		return slices.Contains(f.Permissions.list(k), rule), nil
	}
	r := f.Repos[matchingKey(slices.Collect(maps.Keys(f.Repos)), repoKey)]
	return slices.Contains(r.list(k), rule), nil
}

// LineError is a mistake in the hand-edited file, by line where it has one.
type LineError struct {
	Line int
	Msg  string
	// Detail may quote the rule or a key, and stays out of a report.
	Detail string
}

// CheckFile reports what is wrong with permissions.json, by line. A rule that
// the filter drops is a mistake too, as it would never apply.
func CheckFile(src []byte) []LineError {
	f, err := ParseAggyFile(src)
	if err != nil {
		return []LineError{decodeError(src, err)}
	}
	var out []LineError
	for _, k := range UnknownKeys(src) {
		last := k[strings.LastIndexAny(k, ". ")+1:]
		out = append(out, LineError{lineOf(src, `"`+last+`"`), "aggy does not use this key", k})
	}
	if m := f.Permissions.DefaultMode; m != "" {
		if _, err := CheckMode(m); err != nil {
			out = append(out, LineError{lineOf(src, `"defaultMode"`), "defaultMode is not a mode aggy passes on", err.Error()})
		}
	}
	check := func(k Kind, rules []string) {
		for _, r := range rules {
			quoted, _ := json.Marshal(r)
			line := lineOf(src, string(quoted))
			if k == Allow {
				if class, why := CheckAllow(r); class == Drop {
					out = append(out, LineError{line, "this allow rule is never passed on", r + ": " + why})
				}
				continue
			}
			if _, _, _, err := ParseRule(r); err != nil {
				out = append(out, LineError{line, "this is not a rule", r + ": " + err.Error()})
			}
		}
	}
	check(Deny, f.Permissions.Deny)
	check(Ask, f.Permissions.Ask)
	check(Allow, f.Permissions.Allow)
	policy := func(m map[string]string) {
		for _, g := range slices.Sorted(maps.Keys(m)) {
			if err := CheckPolicySetting(g, m[g]); err != nil {
				quoted, _ := json.Marshal(g)
				out = append(out, LineError{lineOf(src, string(quoted)), "this policy setting is not one aggy knows, so a launch refuses", err.Error()})
			}
		}
	}
	policy(f.Policy)
	for _, key := range slices.Sorted(maps.Keys(f.Repos)) {
		r := f.Repos[key]
		quoted, _ := json.Marshal(key)
		if !strings.HasPrefix(key, "/") {
			out = append(out, LineError{lineOf(src, string(quoted)), "a repo here is named by its absolute path", key})
		}
		check(Deny, r.Deny)
		check(Ask, r.Ask)
		check(Allow, r.Allow)
		policy(r.Policy)
	}
	return out
}

// decodeError finds the line of a JSON error.
func decodeError(src []byte, err error) LineError {
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syn):
		return LineError{lineAt(src, syn.Offset), "this is not valid JSON", err.Error()}
	case errors.As(err, &typ):
		return LineError{lineAt(src, typ.Offset), "a value here has the wrong type", err.Error()}
	}
	return LineError{0, "this is not valid JSON", err.Error()}
}

func lineAt(src []byte, offset int64) int {
	if offset > int64(len(src)) {
		offset = int64(len(src))
	}
	if offset < 0 {
		offset = 0
	}
	return bytes.Count(src[:offset], []byte("\n")) + 1
}

func lineOf(src []byte, text string) int {
	i := bytes.Index(src, []byte(text))
	if i < 0 {
		return 0
	}
	return bytes.Count(src[:i], []byte("\n")) + 1
}
