package parity

import (
	"errors"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestAddCodexTrust(t *testing.T) {
	const dir = "/Users/me/repo"
	for _, tc := range []struct {
		name, src, want string
		err             bool
	}{
		{name: "empty", src: "", want: "[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\n"},
		{name: "keeps what is there", src: "# mine\nmodel = \"x\"\n\n[tui]\ntheme = \"y\" # dark\n",
			want: "# mine\nmodel = \"x\"\n\n[tui]\ntheme = \"y\" # dark\n\n[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\n"},
		{name: "no final newline", src: "model = \"x\"", want: "model = \"x\"\n\n[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\n"},
		{name: "another project", src: "[projects.\"/other\"]\ntrust_level = \"untrusted\"\n",
			want: "[projects.\"/other\"]\ntrust_level = \"untrusted\"\n\n[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\n"},
		{name: "already", src: "[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\n", want: "[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\n"},
		{name: "own table without the key", src: "[projects.\"/Users/me/repo\"]\nother = 1\n", want: "[projects.\"/Users/me/repo\"]\ntrust_level = \"trusted\"\nother = 1\n"},
		{name: "says otherwise", src: "[projects.\"/Users/me/repo\"]\ntrust_level = \"untrusted\"\n", err: true},
		{name: "inline table", src: "projects = { \"/other\" = { trust_level = \"trusted\" } }\n", err: true},
		{name: "inline entry", src: "[projects]\n\"/Users/me/repo\" = { other = 1 }\n", err: true},
		{name: "bad toml", src: "= nope", err: true},
		{name: "wrong type", src: "projects = 3\n", err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AddCodexTrust([]byte(tc.src), dir)
			if (err != nil) != tc.err {
				t.Fatalf("err %v", err)
			}
			if !tc.err && string(got) != tc.want {
				t.Errorf("got %q", got)
			}
			if err == nil {
				again, err := AddCodexTrust(got, dir)
				if err != nil || string(again) != string(got) {
					t.Errorf("not idempotent: %q, %v", again, err)
				}
			}
		})
	}
}

func TestAddCodexTrustConflictAndQuoting(t *testing.T) {
	if _, err := AddCodexTrust([]byte("[projects.\"/a\"]\ntrust_level = \"untrusted\"\n"), "/a"); !errors.Is(err, ErrCodexTrustConflict) {
		t.Errorf("%v", err)
	}
	if _, err := AddCodexTrust(nil, "/a\nb"); err == nil {
		t.Error("a line break must not reach the file")
	}
	const odd = `/a"b\c] x`
	got, err := AddCodexTrust(nil, odd)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Projects map[string]struct {
			TrustLevel string `toml:"trust_level"`
		} `toml:"projects"`
	}
	if err := toml.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Projects[odd].TrustLevel != "trusted" || strings.Count(string(got), "[projects") != 1 {
		t.Errorf("%q", got)
	}
}
