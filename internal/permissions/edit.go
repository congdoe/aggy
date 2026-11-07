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

// list returns the rules of a kind.
func (o *Object) list(k Kind) *[]string {
	switch k {
	case Allow:
		return &o.Allow
	case Ask:
		return &o.Ask
	}
	return &o.Deny
}

func (r *RepoRules) list(k Kind) *[]string {
	switch k {
	case Allow:
		return &r.Allow
	case Ask:
		return &r.Ask
	}
	return &r.Deny
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
// needs. The file has no comments, so encoding it again loses only spacing.
func (e Edit) Apply(src []byte) ([]byte, error) {
	var f AggyFile
	if len(src) > 0 {
		var err error
		if f, err = ParseAggyFile(src); err != nil {
			return nil, fmt.Errorf("%s is not valid: %w; aggy doctor says where", FileName, err)
		}
	}
	list := f.Permissions.list(e.Kind)
	var repo RepoRules
	if e.RepoKey != "" {
		repo = f.Repos[e.RepoKey]
		list = repo.list(e.Kind)
	}
	if i := slices.Index(*list, e.Rule); e.Remove && i >= 0 {
		*list = slices.Delete(*list, i, i+1)
	} else if !e.Remove && i < 0 {
		*list = append(*list, e.Rule)
	} else {
		return src, nil
	}
	if e.RepoKey != "" {
		if f.Repos == nil {
			f.Repos = map[string]RepoRules{}
		}
		if len(repo.Allow)+len(repo.Ask)+len(repo.Deny) == 0 {
			delete(f.Repos, e.RepoKey)
		} else {
			f.Repos[e.RepoKey] = repo
		}
	}
	out, err := json.MarshalIndent(f, "", "  ")
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
		return slices.Contains(*f.Permissions.list(k), rule), nil
	}
	r := f.Repos[repoKey]
	return slices.Contains(*r.list(k), rule), nil
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
	for _, key := range slices.Sorted(maps.Keys(f.Repos)) {
		r := f.Repos[key]
		quoted, _ := json.Marshal(key)
		if !strings.HasPrefix(key, "/") {
			out = append(out, LineError{lineOf(src, string(quoted)), "a repo here is named by its absolute path", key})
		}
		check(Deny, r.Deny)
		check(Ask, r.Ask)
		check(Allow, r.Allow)
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
	// encoding/json names an unknown field in its text only.
	if name, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		return LineError{lineOf(src, name), "aggy does not know this key", err.Error()}
	}
	return LineError{0, "this is not valid", err.Error()}
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
