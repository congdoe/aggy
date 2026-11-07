package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var tagLine = regexp.MustCompile(`(?m) \[[a-z_]+\.[a-z_]+ · run [0-9A-Z]{26}\]$`)

// TestEveryRefusalEndsWithCode (F14): each refusal and error names its code
// and run, and exits non-zero. The cases are the ones found untagged, and one
// for each command that had a path of its own.
func TestEveryRefusalEndsWithCode(t *testing.T) {
	tests := []struct {
		args []string
		exit int
		code string
	}{
		{[]string{"frobnicate"}, 2, "cli.usage"},
		{[]string{"run", "frob"}, 2, "cli.usage"},
		{[]string{"run"}, 2, "cli.usage"},
		{[]string{"run", "--frob", "claude"}, 2, "cli.usage"},
		{[]string{"new", "frob"}, 2, "cli.usage"},
		{[]string{"new", "--frob"}, 2, "cli.usage"},
		{[]string{"resume", "nosuchid"}, 2, "cli.usage"},
		{[]string{"resume", "--frob"}, 2, "cli.usage"},
		{[]string{"sessions", "x"}, 2, "cli.usage"},
		{[]string{"session", "x"}, 2, "cli.usage"},
		{[]string{"--continue", "x"}, 2, "cli.usage"},
		{[]string{"unwire"}, 2, "cli.usage"},
		{[]string{"unwire", "--frob"}, 2, "cli.usage"},
		{[]string{"unwire", "--change", "nosuch"}, 1, "cli.not_found"},
		{[]string{"unwire", "--all", "x"}, 2, "cli.usage"},
		{[]string{"unwire", "--all", "--force"}, 2, "cli.usage"},
		{[]string{"unwire", "--all", "--yes"}, 2, "cli.usage"},
		{[]string{"inbox", "--frob"}, 2, "cli.usage"},
		{[]string{"inbox", "nosuchid"}, 2, "cli.usage"},
		{[]string{"inbox", "abcd1234"}, 1, "cli.not_found"},
		{[]string{"image", "x"}, 2, "cli.usage"},
		{[]string{"image", "--edit", "nosuch.png", "x"}, 1, "cli.not_found"},
		{[]string{"video", "--frob"}, 2, "cli.usage"},
		{[]string{"attribution"}, 2, "cli.usage"},
		{[]string{"attribution", "wire", "--frob"}, 2, "cli.usage"},
		{[]string{"attribution", "wire"}, 1, "cli.not_in_repo"},
		{[]string{"diag", "frob"}, 2, "cli.usage"},
		{[]string{"diag", "show"}, 2, "cli.usage"},
		{[]string{"log", "--frob"}, 2, "cli.usage"},
		{[]string{"log", "x"}, 2, "cli.usage"},
		{[]string{"stats", "--frob"}, 2, "cli.usage"},
		{[]string{"usage", "--frob"}, 2, "cli.usage"},
		{[]string{"brief", "--frob"}, 2, "cli.usage"},
		{[]string{"brief"}, 2, "cli.usage"},
		{[]string{"skill", "frob"}, 2, "cli.usage"},
		{[]string{"skill", "link", "--vendor", "frob"}, 2, "cli.usage"},
		{[]string{"skill", "status", "--dry-run"}, 2, "cli.usage"},
		{[]string{"hooks", "frob"}, 2, "cli.usage"},
		{[]string{"hooks", "install", "--frob"}, 2, "cli.usage"},
		{[]string{"tool", "frob"}, 2, "cli.usage"},
		{[]string{"memory", "frob"}, 2, "cli.usage"},
		{[]string{"memory", "import", "--frob"}, 2, "cli.usage"},
		{[]string{"policy", "frob"}, 2, "cli.usage"},
		{[]string{"policy", "wire", "--frob"}, 2, "cli.usage"},
		{[]string{"mcp", "frob"}, 2, "cli.usage"},
		{[]string{"mcp", "wire", "--vendor", "frob"}, 2, "cli.usage"},
		{[]string{"accept"}, 2, "cli.usage"},
		{[]string{"reject"}, 2, "cli.usage"},
		{[]string{"gc", "--frob"}, 2, "cli.usage"},
		{[]string{"setup", "--frob"}, 2, "cli.usage"},
		{[]string{"doctor", "--frob"}, 2, "cli.usage"},
		{[]string{"borrow", "frob", "x"}, 2, "cli.usage"},
		{[]string{"dev", "frob"}, 2, "cli.usage"},
		{[]string{"switch", "--frob"}, 2, "cli.usage"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			isolate(t)
			t.Setenv("PATH", t.TempDir())
			t.Chdir(t.TempDir())
			var out, errb bytes.Buffer
			code := Run(tt.args, &out, &errb)
			if code != tt.exit {
				t.Errorf("exit = %d, want %d; stderr %q", code, tt.exit, errb.String())
			}
			first, _, _ := strings.Cut(errb.String(), "\n")
			if !tagLine.MatchString(first) || !strings.Contains(first, "["+tt.code+" ") {
				t.Errorf("first line %q, want it to end with [%s · run <id>]", first, tt.code)
			}
		})
	}
}

// TestUnwireNotFoundCoded: an id no change has says cli.not_found once the
// index exists.
func TestUnwireNotFoundCoded(t *testing.T) {
	isolate(t)
	sessionStore(t)
	var out, errb bytes.Buffer
	if code := Run([]string{"unwire", "--change", "nosuch"}, &out, &errb); code != 1 {
		t.Errorf("exit = %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "[cli.not_found · run ") {
		t.Errorf("stderr = %q", errb.String())
	}
}

// TestDiagRefusalTaggedNotLogged: aggy diag only reads, so its refusals carry
// the tag and write no record (docs/logging.md).
func TestDiagRefusalTaggedNotLogged(t *testing.T) {
	isolate(t)
	sessionStore(t)
	var out, errb bytes.Buffer
	if code := Run([]string{"diag", "show", "nosuch"}, &out, &errb); code != 1 {
		t.Errorf("exit = %d, stderr %q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "[cli.not_found · run ") {
		t.Errorf("stderr = %q", errb.String())
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("AGGY_HOME"), "diag.db")); err == nil {
		t.Error("aggy diag wrote diag.db")
	}
}

// TestDryRunRefusalUntagged: a dry run writes nothing, so its refusal has
// no tag and no record (docs/logging.md).
func TestDryRunRefusalUntagged(t *testing.T) {
	isolate(t)
	t.Chdir(t.TempDir())
	t.Setenv("PATH", t.TempDir())
	var out, errb bytes.Buffer
	if code := Run([]string{"attribution", "wire", "--dry-run"}, &out, &errb); code != 1 {
		t.Errorf("exit = %d, stderr %q", code, errb.String())
	}
	if strings.Contains(errb.String(), " · run ") {
		t.Errorf("stderr = %q, want no tag", errb.String())
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("AGGY_HOME"), "diag.db")); err == nil {
		t.Error("the dry run wrote diag.db")
	}
}

// TestNoUntaggedFailurePrint: a print to stderr in internal/cli either has a
// tag, is usage text after a tagged line, or is one of the named kinds that
// are no refusal, so a new error path cannot forget the code.
func TestNoUntaggedFailurePrint(t *testing.T) {
	// Lines allowed by what they print: warnings and notes (the command goes
	// on), and the two prints a tag is built by.
	allowed := []string{
		"warning", "skipped ", "stopped at", "starting ", "this directory was replaced", "cannot be resumed (", "has no recorded session id;",
		"has no recorded session id; starting", "the newer %s session here", "is on for this", "your own ", "probing ", "still present",
		`"%s: %s\n", name, termsafe.Line(`, `"aggy %s: %s\n", cmd, msg`, `"aggy: %s\n", msg`, `"\n%s"`,
		"Nothing changed.", "switchUsage", "near limit uses", "proxy not used", `Fprintln(stderr, n)`, `Fprintln(stderr, line+tag)`, `"aggy hook: %s%s\n"`, `did not answer in time`,
		`Notice: func(s string)`, // a note from the borrow, which goes on
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	print := regexp.MustCompile(`Fprint\w*\(stderr\b`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
	lines:
		for i, l := range strings.Split(string(b), "\n") {
			if !print.MatchString(l) || strings.Contains(l, "Tag(") {
				continue
			}
			for _, a := range allowed {
				if strings.Contains(l, a) {
					continue lines
				}
			}
			t.Errorf("%s:%d prints to stderr with no tag: %s", f, i+1, strings.TrimSpace(l))
		}
	}
}
