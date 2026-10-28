package devfix

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/safefs"
)

// Regression tests for the PR #5 review (item 2). Each fails on ed38103.

func kinds(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Kind)
	}
	return out
}

// TestDetectFailsBinaryFixtures: a file with a NUL byte or invalid UTF-8
// is neither scrubbed nor read, so it fails unless allowlisted.
func TestDetectFailsBinaryFixtures(t *testing.T) {
	root := t.TempDir()
	write(t, root, "nul.txt", "ok\x00 /Users/ann")
	write(t, root, "latin1.txt", "caf\xe9 /Users/ann")
	got, err := Detect(root, Identity{}, nil)
	if err != nil || !slices.Equal(kinds(got), []string{KindBinary, KindBinary}) {
		t.Errorf("Detect = %v, %v", got, err)
	}
	allow, _ := ParseAllowlist([]byte("*.txt\t" + BinaryLiteral + "\n"))
	if got, _ := Detect(root, Identity{}, allow); len(got) != 0 {
		t.Errorf("allowlisted binaries = %v", got)
	}
}

// TestEncodedHomePaths: URL-encoded homes and Claude's dash form are found
// and scrubbed.
func TestEncodedHomePaths(t *testing.T) {
	id := Identity{Home: "/opt/people/ann"}
	for _, line := range []string{
		"sessions/%2FUsers%2Fann%2Fproj/x",
		"sessions/%2fhome%2fbob%2fproj/x",
		"projects/-Users-ann-proj/s.jsonl",
		`"dir": "-home-bob-work"`,
		"cwd=%2Fopt%2Fpeople%2Fann%2Fproj",
		"projects/-opt-people-ann-proj",
	} {
		if got := DetectText("f", line, id, nil); !slices.Contains(kinds(got), KindHome) {
			t.Errorf("DetectText(%q) = %v, want a home path", line, got)
		}
		if out := NewScrubber(id).String(line); len(DetectText("f", out, id, nil)) != 0 {
			t.Errorf("scrubbed %q = %q, still flagged", line, out)
		}
	}
}

// TestTokenPatterns: every listed token shape is found and scrubbed.
func TestTokenPatterns(t *testing.T) {
	for _, tok := range []string{
		"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEYab",
		"ASIAIOSFODNN7EXAMPLE",
		"AKIAIOSFODNN7EXAMPLE",
		"AIzaSyA1234567890abcdefghijklmnopqrstuv",
		"ya29.a0AfH6SMBx1234567890",
		"Authorization: Basic dXNlcjpwYXNz",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY-----",
		"rk_live_1234567890abcdef",
		"sk_live_1234567890abcdef",
		"eyJhbGciOiJub25lIn0.eyJzdWIiOiIxIn0.",
		"ghp_1234567890abcdefghij",
		"sk-proj-1234567890abcdef",
		"xai-1234567890abcdef",
	} {
		if got := DetectText("f", "x "+tok+" y", Identity{}, nil); !slices.Contains(kinds(got), KindToken) {
			t.Errorf("%q not found: %v", tok, got)
		}
		if out := NewScrubber(Identity{}).String(tok); strings.Contains(out, tok) {
			t.Errorf("%q not scrubbed: %q", tok, out)
		}
	}
}

// TestDetectIdentityAndUUIDs: the user and host names, in any case, and
// UUIDs other than the placeholders fail the check.
func TestDetectIdentityAndUUIDs(t *testing.T) {
	id := Identity{User: "annb", Host: "Ann-MBP.local"}
	got := DetectText("f", "by ANNB@ann-mbp, id 0b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e and "+UUIDPrefix+"000000000001", id, nil)
	if !slices.Equal(kinds(got), []string{KindUUID, KindUser, KindHost}) {
		t.Errorf("DetectText = %v", got)
	}
	out := NewScrubber(id).String("by ANNB@ann-mbp")
	if out != "by "+UserPlaceholder+"@"+HostPlaceholder {
		t.Errorf("scrubbed = %q", out)
	}
}

// TestScrubDirAllOrNothing (10): a file scrub cannot read stops it before
// any file is written.
func TestScrubDirAllOrNothing(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.txt", "/Users/ann/x")
	big := make([]byte, MaxFixtureFile+1)
	if err := safefs.WriteFile(filepath.Join(root, "z.txt"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ScrubDir(root, NewScrubber(Identity{})); err == nil {
		t.Fatal("ScrubDir succeeded with an oversized file")
	}
	if b, _ := safefs.ReadFile(filepath.Join(root, "a.txt")); string(b) != "/Users/ann/x" {
		t.Errorf("a.txt was rewritten: %q", b)
	}
}
