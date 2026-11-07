package permissions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEditApply(t *testing.T) {
	add := Edit{Kind: Allow, Rule: "Bash(ls)"}
	b, err := add.Apply(nil)
	if err != nil {
		t.Fatal(err)
	}
	if has, _ := Has(b, Allow, "Bash(ls)", ""); !has {
		t.Errorf("%s", b)
	}
	again, err := add.Apply(b)
	if err != nil || string(again) != string(b) {
		t.Errorf("not idempotent: %s %v", again, err)
	}
	repo := Edit{Kind: Deny, Rule: "Bash(rm x)", RepoKey: "/r"}
	if b, err = repo.Apply(b); err != nil {
		t.Fatal(err)
	}
	if has, _ := Has(b, Deny, "Bash(rm x)", "/r"); !has {
		t.Errorf("%s", b)
	}
	if has, _ := Has(b, Deny, "Bash(rm x)", ""); has {
		t.Errorf("wrong scope: %s", b)
	}
	repo.Remove = true
	if b, err = repo.Apply(b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "repos") {
		t.Errorf("an empty repo stays: %s", b)
	}
	add.Remove = true
	if b, err = add.Apply(b); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "Bash") {
		t.Errorf("%s", b)
	}
	if _, err := add.Apply([]byte("{bad")); err == nil {
		t.Error("a broken file must not be overwritten")
	}
}

func TestCheckFileLines(t *testing.T) {
	if got := CheckFile([]byte(`{"permissions":{"allow":["Bash(ls)"]}}`)); len(got) != 0 {
		t.Errorf("%+v", got)
	}
	for _, tc := range []struct {
		name, text string
		line       int
	}{
		{"syntax", "{\n  \"permissions\": {\n    \"allow\": [\"x\",]\n  }\n}\n", 3},
		{"unknown key", "{\n  \"permissions\": {\n    \"alow\": []\n  }\n}\n", 3},
		{"wrong type", "{\n  \"permissions\": {\n    \"allow\": 3\n  }\n}\n", 3},
		{"dropped rule", "{\n  \"permissions\": {\n    \"allow\": [\n      \"Bash(*)\"\n    ]\n  }\n}\n", 4},
		{"bad mode", "{\n  \"permissions\": {\n    \"defaultMode\": \"bypassPermissions\"\n  }\n}\n", 3},
		{"bad deny", "{\n  \"permissions\": {\n    \"deny\": [\n      \" x\"\n    ]\n  }\n}\n", 4},
		{"relative repo", "{\n  \"repos\": {\n    \"here\": {}\n  }\n}\n", 3},
	} {
		got := CheckFile([]byte(tc.text))
		if len(got) != 1 || got[0].Line != tc.line {
			t.Errorf("%s: %+v", tc.name, got)
		}
	}
}

func TestEditKeepsKeysAndSpelling(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	key := canonical(dir)
	src := []byte(`{"$schema":"https://x","repos":{"` + link + `":{"allow":["Bash(ls)"]}}}`)
	if has, err := Has(src, Allow, "Bash(ls)", key); err != nil || !has {
		t.Fatalf("has: %v %v", has, err)
	}
	out, err := Edit{Kind: Deny, Rule: "Bash(x y)", RepoKey: key}.Apply(src)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "$schema") || strings.Count(string(out), `"Bash(ls)"`) != 1 {
		t.Errorf("%s", out)
	}
	if has, _ := Has(out, Deny, "Bash(x y)", key); !has {
		t.Errorf("not added: %s", out)
	}
	out, err = Edit{Kind: Allow, Rule: "Bash(ls)", RepoKey: key, Remove: true}.Apply(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "Bash(ls)") {
		t.Errorf("not removed: %s", out)
	}
}
