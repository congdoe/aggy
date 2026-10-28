package devfix

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/safefs"
)

func TestScrubString(t *testing.T) {
	s := NewScrubber(Identity{Home: "/opt/people/ann", User: "annb", Host: "ann-mbp.local"})
	id1 := "0B9D3C4E-1f2a-4b5c-8d7e-6f5a4b3c2d1e"
	id2 := "11111111-2222-4333-8444-555555555555"
	tests := []struct{ in, want string }{
		{"cwd /opt/people/ann/proj", "cwd $HOME/proj"},
		{"/Users/ann/x and /home/bob/y and /root/z", "$HOME/x and $HOME/y and $HOME/z"},
		{`C:\Users\Ann Lee\proj`, `$HOME\proj`},
		{"mail ann@corp.io or doc@example.com", "mail user@example.com or doc@example.com"},
		{"key sk-ant-api03-abcdefghijklmnopqrstuvwxyz", "key " + TokenPlaceholder},
		{"auth: Bearer abcdefghijklmnopqrstuvwxyz0123", "auth: " + TokenPlaceholder},
		{`{"access_token": "abcdefghijklmnop1234"}`, `{` + TokenPlaceholder + `"}`},
		{"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijklmnop", "jwt " + TokenPlaceholder},
		{"t " + id1 + " " + id2 + " " + strings.ToLower(id1), "t 00000000-0000-4000-8000-000000000001 00000000-0000-4000-8000-000000000002 00000000-0000-4000-8000-000000000001"},
		{"user annb@ann-mbp: (ann-mbp.local) annb", "user fixture-user@fixture-host: (fixture-host) annb"},
		{"annbell is someone else", "annbell is someone else"},
	}
	for _, tt := range tests {
		if got := s.String(tt.in); got != tt.want {
			t.Errorf("String(%q)\n got %q\nwant %q", tt.in, got, tt.want)
		}
	}
}

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := safefs.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := safefs.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectAndScrubDir(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/stdout", "path /Users/ann/x\nmail ann@corp.io\nkey xai-abcdefghijklmnopqrstuvwxyz\n")
	write(t, root, "b/ok.json", `{"id":"0b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e","mail":"x@example.org"}`)
	write(t, root, "c/home/sam/name.txt", "fine")
	write(t, root, "d/image.png", "\x89PNG\x00 /Users/ann")
	got, err := Detect(root, Identity{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, f := range got {
		kinds = append(kinds, f.File+":"+f.Kind)
	}
	want := []string{"a/stdout:" + KindHome, "a/stdout:" + KindEmail, "a/stdout:" + KindToken, "b/ok.json:" + KindUUID,
		"c/home/sam/name.txt:" + KindHome, "d/image.png:" + KindBinary}
	if !slices.Equal(kinds, want) {
		t.Errorf("Detect = %v, want %v", got, want)
	}

	allow, err := ParseAllowlist([]byte("# documented\na/*\tann@corp.io\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, _ = Detect(root, Identity{}, allow)
	for _, f := range got {
		if f.Kind == KindEmail {
			t.Errorf("allowlisted email reported: %v", f)
		}
	}
	if _, err := ParseAllowlist([]byte("no tab here\n")); err == nil {
		t.Error("malformed allowlist accepted")
	}

	changed, err := ScrubDir(root, NewScrubber(Identity{}))
	if err != nil || !slices.Equal(changed, []string{"a/stdout", "b/ok.json"}) {
		t.Errorf("ScrubDir = %v, %v", changed, err)
	}
	got, _ = Detect(root, Identity{}, nil)
	if len(got) != 2 || got[0].Line != 0 || got[1].Kind != KindBinary {
		t.Errorf("after scrub = %v, want only the path and binary findings", got)
	}
}
