package devfix

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/congdoe/aggy/internal/safefs"
)

// Regression tests for the second PR #5 review. Each fails on bc0daa5.

// TestRunnerIdentity: on CI the account is "runner", an ordinary word. The
// fixtures README passes, and runner's home paths are still flagged.
func TestRunnerIdentity(t *testing.T) {
	id := Identity{User: "runner", Home: "/home/runner", Host: ""}
	b, err := safefs.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if got := DetectText("README.md", string(b)+"\nthe test runner ran\n", id, []Allow{{"README.md", "/root"}}); len(got) != 0 {
		t.Errorf("README with user runner: %v", got)
	}
	for _, line := range []string{"/home/runner/x", "/Users/runner", "-home-runner-work-aggy"} {
		if got := DetectText("f", line, id, nil); !slices.Contains(kinds(got), KindHome) {
			t.Errorf("%q not flagged: %v", line, got)
		}
	}
	// A generic name is never identifying on its own.
	for _, u := range []string{"root", "ubuntu", "user", "admin", "vscode", "codespace", "build", "ab"} {
		if got := DetectText("f", "the "+u+"@ci and "+u, Identity{User: u}, nil); slices.Contains(kinds(got), KindUser) {
			t.Errorf("%s flagged as a user name: %v", u, got)
		}
	}
	// An identifying name counts only as user@host or an email local part.
	id = Identity{User: "annb"}
	if got := DetectText("f", "annb wrote this", id, nil); len(got) != 0 {
		t.Errorf("bare word flagged: %v", got)
	}
	if got := DetectText("f", "annb@laptop:~$", id, nil); !slices.Contains(kinds(got), KindUser) {
		t.Errorf("user@host not flagged: %v", got)
	}
}

// TestFixturesPassAsRunner runs the fixture check as GitHub's runners
// would: USER=runner and HOSTNAME empty.
func TestFixturesPassAsRunner(t *testing.T) {
	t.Setenv("USER", "runner")
	t.Setenv("HOSTNAME", "")
	t.Setenv("HOME", "/home/runner")
	TestFixturesHoldNoSecrets(t)
}

// TestNestedEncodings (N4): %252F, JSON-escaped slashes, and a dotted user
// name in Claude's dash form are found and scrubbed whole.
func TestNestedEncodings(t *testing.T) {
	id := Identity{User: "john.doe", Home: "/Users/john.doe"}
	for _, line := range []string{
		"sessions/%252FUsers%252Fann%252Fproj",
		`{"cwd":"\/Users\/ann\/proj"}`,
		"projects/-Users-john-doe-proj/s.jsonl",
	} {
		if got := DetectText("f", line, Identity{}, nil); !slices.Contains(kinds(got), KindHome) {
			t.Errorf("%q not flagged: %v", line, got)
		}
		out := NewScrubber(id).String(line)
		if got := DetectText("f", out, id, nil); len(got) != 0 {
			t.Errorf("scrubbed %q = %q, still %v", line, out, got)
		}
	}
	if out := NewScrubber(id).String("projects/-Users-john-doe-proj/"); out != "projects/$HOME-proj/" {
		t.Errorf("dash form = %q", out)
	}
}
