package claudetrust

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const dir = "/work/repo"

// TestSet: the key is set by a minimal edit, whatever the file held of the
// folder, and nothing else in the file changes.
func TestSet(t *testing.T) {
	cases := []struct {
		name   string
		src    string
		before Before
		same   bool // the file already trusts the folder
	}{
		{"no projects", `{"numStartups": 12345678901234567890, "oauthAccount": {"x": "y"}}`, NoProjects, false},
		{"empty file object", `{}`, NoProjects, false},
		{"no entry", `{"projects": {"/other": {"hasTrustDialogAccepted": true}}, "z": [1, 2]}`, NoEntry, false},
		{"empty projects", `{"projects": {}}`, NoEntry, false},
		{"no key", "{\n  \"projects\": {\n    \"/work/repo\": {\n      \"allowedTools\": [],\n      \"history\": [\"a\"]\n    }\n  }\n}\n", NoKey, false},
		{"empty entry", `{"projects": {"/work/repo": {}}}`, NoKey, false},
		{"false", `{"projects": {"/work/repo": {"hasTrustDialogAccepted": false, "k": 1.50}}}`, WasFalse, false},
		{"true", `{"projects": {"/work/repo": {"hasTrustDialogAccepted": true}}}`, WasTrue, true},
		{"a longer path shares the prefix", `{"projects": {"/work/repo2": {"hasTrustDialogAccepted": false}}}`, NoEntry, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, err := State([]byte(c.src), dir)
			if err != nil || before != c.before {
				t.Fatalf("State = %q, %v; want %q", before, err, c.before)
			}
			out, err := Set([]byte(c.src), dir)
			if err != nil {
				t.Fatal(err)
			}
			if c.same != (string(out) == c.src) {
				t.Fatalf("changed = %v, want %v: %s", string(out) != c.src, !c.same, out)
			}
			if now, _ := State(out, dir); now != WasTrue {
				t.Errorf("after Set: %q", now)
			}
			// Everything else is as it was: the other keys decode the same,
			// a long number keeps all its digits.
			want, got := decodeT(t, c.src), decodeT(t, string(out))
			delete(entryOf(got), Key)
			if e := entryOf(want); e != nil {
				delete(e, Key)
			}
			prune(got, want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("something else changed:\n%s\nwas\n%s", out, c.src)
			}
			if strings.Contains(c.src, "12345678901234567890") && !strings.Contains(string(out), "12345678901234567890") {
				t.Error("a long number lost digits")
			}
			if again, _ := Set(out, dir); !bytes.Equal(again, out) {
				t.Error("Set is not idempotent")
			}
		})
	}
}

// prune drops from got an empty entry and projects Set made, where want
// had none, so the comparison is of the rest.
func prune(got, want map[string]any) {
	p, _ := got["projects"].(map[string]any)
	if p == nil {
		return
	}
	if e, ok := p[dir].(map[string]any); ok && len(e) == 0 {
		if wp, _ := want["projects"].(map[string]any); wp == nil || wp[dir] == nil {
			delete(p, dir)
		}
	}
	if len(p) == 0 && want["projects"] == nil {
		delete(got, "projects")
	}
}

func decodeT(t *testing.T, s string) map[string]any {
	t.Helper()
	d, err := decode([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func entryOf(doc map[string]any) map[string]any {
	p, _ := doc["projects"].(map[string]any)
	e, _ := p[dir].(map[string]any)
	return e
}

// TestRefused: a file aggy cannot read the key in, or would have to guess at,
// is refused, never rewritten.
func TestRefused(t *testing.T) {
	for _, src := range []string{
		``,
		`not json`,
		`[]`,
		`{"projects": []}`,
		`{"projects": {"/work/repo": "x"}}`,
		`{"projects": {"/work/repo": {"hasTrustDialogAccepted": "yes"}}}`,
		`{"projects": {"/work/repo": {}}, "projects": {}}`,
		`{"projects": {"/work/repo": {"hasTrustDialogAccepted": false, "hasTrustDialogAccepted": true}}}`,
		`{"a": 1} {"b": 2}`,
	} {
		if out, err := Set([]byte(src), dir); err == nil {
			t.Errorf("Set(%q) = %q, want a refusal", src, out)
		}
	}
	if _, err := Set([]byte(`{}`), "relative"); err == nil {
		t.Error("a relative folder was taken")
	}
	if _, err := Set([]byte(`{}`), "/a\nb"); err == nil {
		t.Error("a folder with a line break was taken")
	}
}

// TestUndo: unwire puts back the one key as it was, and leaves what Claude
// wrote since: an entry or projects aggy made goes only while it holds
// nothing else.
func TestUndo(t *testing.T) {
	cases := []struct {
		name  string
		src   string
		since func(map[string]any) // what Claude wrote after aggy's Set
		want  string               // the document after Undo, as JSON
	}{
		{"no projects, nothing since", `{"a": 1}`, nil, `{"a": 1}`},
		{"no projects, Claude added to the entry", `{"a": 1}`, func(d map[string]any) {
			entryOf(d)["history"] = []any{"x"}
		}, `{"a": 1, "projects": {"/work/repo": {"history": ["x"]}}}`},
		{"no projects, Claude added another folder", `{"a": 1}`, func(d map[string]any) {
			d["projects"].(map[string]any)["/b"] = map[string]any{}
		}, `{"a": 1, "projects": {"/b": {}}}`},
		{"no entry", `{"projects": {"/b": {"x": 1}}}`, func(d map[string]any) { d["new"] = true },
			`{"new": true, "projects": {"/b": {"x": 1}}}`},
		{"no key", `{"projects": {"/work/repo": {"y": 2}}}`, nil, `{"projects": {"/work/repo": {"y": 2}}}`},
		{"no key, entry otherwise empty", `{"projects": {"/work/repo": {}}}`, nil, `{"projects": {"/work/repo": {}}}`},
		{"was false", `{"projects": {"/work/repo": {"hasTrustDialogAccepted": false}}}`, func(d map[string]any) { d["n"] = 3 },
			`{"n": 3, "projects": {"/work/repo": {"hasTrustDialogAccepted": false}}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, err := State([]byte(c.src), dir)
			if err != nil {
				t.Fatal(err)
			}
			out, err := Set([]byte(c.src), dir)
			if err != nil {
				t.Fatal(err)
			}
			if c.since != nil {
				// Claude rewrites the file whole, its own way.
				d := decodeT(t, string(out))
				c.since(d)
				if out, err = json.MarshalIndent(d, "", "  "); err != nil {
					t.Fatal(err)
				}
			}
			undone, err := Undo(out, dir, before)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := decodeT(t, string(undone)), decodeT(t, c.want); !reflect.DeepEqual(got, want) {
				t.Errorf("after Undo:\n%s\nwant\n%s", undone, c.want)
			}
			// The reverse names it, and reads back.
			b, f, ok := ParseReverse(Reverse(before, dir))
			if !ok || b != before || f != dir {
				t.Errorf("ParseReverse(Reverse) = %q %q %v", b, f, ok)
			}
		})
	}
	// A key Claude or the person set false since is theirs: left alone.
	src := `{"projects": {"/work/repo": {"hasTrustDialogAccepted": false}}}`
	if out, err := Undo([]byte(src), dir, NoEntry); err != nil || string(out) != src {
		t.Errorf("Undo over a later false: %q, %v", out, err)
	}
}

func TestParseReverse(t *testing.T) {
	for _, rev := range []string{"", "aggy-hooks", "claude-trust:", "claude-trust:true:/a", "claude-trust:nokey:rel", "claude-trust:bogus:/a"} {
		if _, _, ok := ParseReverse(rev); ok {
			t.Errorf("ParseReverse(%q) took it", rev)
		}
	}
	if b, f, ok := ParseReverse("claude-trust:nokey:/a:b"); !ok || b != NoKey || f != "/a:b" {
		t.Errorf("a folder with a colon: %q %q %v", b, f, ok)
	}
}

func TestPath(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, c := range []struct {
		env  map[string]string
		want string
		ok   bool
	}{
		{map[string]string{"HOME": "/h"}, "/h/.claude.json", true},
		{map[string]string{"HOME": "/h", "CLAUDE_CONFIG_DIR": "/c"}, "/c/.claude.json", true},
		{map[string]string{"HOME": "/h", "CLAUDE_CONFIG_DIR": "c"}, "", false},
		{map[string]string{}, "", false},
	} {
		got, err := Path(env(c.env))
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("Path(%v) = %q, %v", c.env, got, err)
		}
	}
}
