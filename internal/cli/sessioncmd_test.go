package cli

import (
	"io"
	"strings"
	"testing"
)

// TestSessionAggyRefuses (T6909): the in-session runner takes only the commands
// a typed command maps to, never one that changes config wholesale, starts a
// borrow, or a proxy, and never reads the terminal.
func TestSessionAggyRefuses(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("AGGY_HOME", t.TempDir())
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"unwire"}, "not a command for inside a session"},
		{[]string{"borrow", "research", "x"}, "not a command for inside a session"},
		{[]string{"tool", "add", "x"}, "takes on, off, or status here"},
		{[]string{"trust", "/"}, "takes only this folder"},
		{[]string{"trust", "."}, "/aggy trust opens the list"},
		{[]string{"permissions", "add", "Bash(x)"}, "change them in a terminal"},
		{[]string{"diag", "sql", "select 1"}, "shows the log or one run"},
		{[]string{"accept", "--help"}, "Usage: aggy accept"},
	} {
		got := sessionAggy(c.args, false)
		if !strings.Contains(got, c.want) {
			t.Errorf("sessionAggy(%q) = %q, want %q in it", c.args, got, c.want)
		}
		// R121: a refusal carries its code and run id, as every refusal does.
		if c.args[len(c.args)-1] != "--help" && !strings.Contains(got, "[cli.usage · run ") {
			t.Errorf("sessionAggy(%q) = %q, with no [cli.usage · run <id>] tag", c.args, got)
		}
	}
}

// TestSessionAllow (T6978): /aggy allow is aggy permissions add allow for this
// folder: the first run asks and answers no, y answers yes; its checks and
// refusals are the command's own, and nothing else of permissions changes
// from a session.
func TestSessionAllow(t *testing.T) {
	isolate(t)
	inRepo(t)
	atKeyboard(t, "")
	ask := []string{"permissions", "add", "allow", "Bash(npm test)"}
	if out := sessionAggy(ask, false); !strings.Contains(out, "[y/N]") || !strings.Contains(out, "Nothing changed") || aggyFileText(t) != "" {
		t.Fatalf("first run: %q", out)
	}
	if out := sessionAggy(ask, true); !strings.Contains(aggyFileText(t), "Bash(npm test)") {
		t.Fatalf("y: %q", out)
	}
	if out := sessionAggy([]string{"permissions", "add", "allow", "Bash(*)"}, true); !strings.Contains(out, "permissions.bad_rule") {
		t.Errorf("a rule aggy permissions refuses: %q", out)
	}
	for _, args := range [][]string{
		{"permissions", "add", "deny", "Bash(x)"},
		{"permissions", "add", "allow", "Bash(x)", "--everywhere"},
		{"permissions", "add", "allow", "--everywhere"},
		{"permissions", "remove", "allow", "Bash(npm test)"},
	} {
		if out := sessionAggy(args, true); !strings.Contains(out, "change them in a terminal") {
			t.Errorf("sessionAggy(%q) = %q", args, out)
		}
	}
}

// TestWithAnswer: a run's question gets the answer given, never the terminal,
// and keyboard is put back.
func TestWithAnswer(t *testing.T) {
	before := keyboard
	for _, c := range []struct {
		answer bool
		want   string
	}{{false, "\n"}, {true, "y\n"}} {
		restore := withAnswer(c.answer)
		b, _ := io.ReadAll(keyboard)
		restore()
		if string(b) != c.want {
			t.Errorf("answer %v: keyboard read %q, want %q", c.answer, b, c.want)
		}
	}
	if keyboard != before {
		t.Fatal("keyboard was not put back")
	}
}

// TestCapWriter: output past the cap is dropped, and said so.
func TestCapWriter(t *testing.T) {
	var b strings.Builder
	w := &capWriter{w: &b, left: 4}
	if n, _ := w.Write([]byte("abcdef")); n != 6 || b.String() != "abcd" || w.left != 0 {
		t.Fatalf("n %d, %q, left %d", n, b.String(), w.left)
	}
}
