package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/home"
)

const demoTool = `name = "demo"
description = "A demo tool"
version = "1.2.3"
scope = "global"

[install]
check = ["demo", "--version"]
install = ["/usr/local/bin/demo-install", "1.2.3"]

[mcp]
vendors = ["claude", "codex"]
argv = ["demo", "serve", "--stdio"]
env = ["DEMO_HOME"]

[[hook]]
vendors = ["grok"]
event = "PostToolUse"
argv = ["demo", "hook"]

[cost]
method = "mcp-schema"
`

// toolHome isolates HOME and the state directory and writes def to a
// file outside both, returning that file and where add would copy it.
func toolHome(t *testing.T, def string) (src, dest string) {
	t.Helper()
	isolate(t)
	src = filepath.Join(t.TempDir(), "proposed.toml")
	writeAt(t, src, def, 0o644)
	return src, filepath.Join(os.Getenv(home.Env), "tools", "demo.toml")
}

func toolRun(in string, tty bool, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := toolWith(args, strings.NewReader(in), tty, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestToolAddPreviewsThenWrites: add shows every command and wiring per
// vendor, and writes only after a yes (or --yes).
func TestToolAddPreviewsThenWrites(t *testing.T) {
	for _, c := range []struct {
		name  string
		in    string
		tty   bool
		yes   bool
		write bool
	}{
		{"no terminal, no --yes", "y\n", false, false, false},
		{"terminal, no answer", "", true, false, false},
		{"terminal, no", "n\n", true, false, false},
		{"terminal, yes", "y\n", true, false, true},
		{"--yes", "", false, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, dest := toolHome(t, demoTool)
			args := []string{"add", src}
			if c.yes {
				args = append(args, "--yes")
			}
			code, out, errOut := toolRun(c.in, c.tty, args...)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			for _, w := range []string{
				`check: "demo" "--version"`,
				`install: "/usr/local/bin/demo-install" "1.2.3"`,
				"claude:\n    MCP server: \"demo\" \"serve\" \"--stdio\"\n      passes these variables from your environment: DEMO_HOME",
				"codex:\n    MCP server: \"demo\" \"serve\" \"--stdio\"",
				"grok:\n    PostToolUse hook: \"demo\" \"hook\"",
				"off until you turn it on",
				dest,
			} {
				if !strings.Contains(out, w) {
					t.Errorf("preview lacks %q:\n%s", w, out)
				}
			}
			if exists(t, dest) != c.write {
				t.Fatalf("written = %v, want %v\n%s", !c.write, c.write, out)
			}
			if !c.write {
				if exists(t, filepath.Dir(dest)) {
					t.Error("a refused add created the tools folder")
				}
				return
			}
			b, err := os.ReadFile(dest)
			if err != nil || string(b) != demoTool {
				t.Fatalf("copied %q, %v", b, err)
			}
			fi, _ := os.Stat(dest)
			if fi.Mode().Perm()&0o077 != 0 {
				t.Errorf("mode %#o, want private", fi.Mode().Perm())
			}
			_, list, _ := toolRun("", false, "list")
			if !strings.Contains(list, "demo") || !strings.Contains(list, "user") || !strings.Contains(list, dest) {
				t.Errorf("list after add:\n%s", list)
			}
		})
	}
}

func TestToolAddRefuses(t *testing.T) {
	for _, c := range []struct {
		name, def string
		setup     func(t *testing.T, dest string)
		want      string
	}{
		{"invalid", strings.Replace(demoTool, `"demo", "serve", "--stdio"`, `"bash", "-c", "demo serve"`, 1), nil, "proposed.toml: mcp.argv"},
		{"secret env", strings.Replace(demoTool, `"DEMO_HOME"`, `"DEMO_API_KEY"`, 1), nil, "mcp.env[0]"},
		{"default on", "default = true\n" + demoTool, nil, "proposed.toml: default"},
		{"shadows a built-in", strings.Replace(demoTool, `name = "demo"`, `name = "context7"`, 1), nil, "built-in"},
		{"already added", demoTool, func(t *testing.T, dest string) { writeAt(t, dest, demoTool, 0o600) }, "remove it first"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, dest := toolHome(t, c.def)
			if c.setup != nil {
				c.setup(t, dest)
			}
			before, _ := os.ReadFile(dest)
			code, out, errOut := toolRun("y\n", true, "add", src, "--yes")
			if code != 1 || !strings.Contains(errOut, c.want) {
				t.Fatalf("exit %d, stderr %q, want %q", code, errOut, c.want)
			}
			if strings.Contains(out, "MCP server") {
				t.Error("a refused definition was previewed")
			}
			if after, _ := os.ReadFile(dest); !bytes.Equal(before, after) {
				t.Error("a refused add changed the tools folder")
			}
		})
	}
}

func TestToolRemove(t *testing.T) {
	_, dest := toolHome(t, demoTool)
	writeAt(t, dest, demoTool, 0o600)

	if code, _, errOut := toolRun("", false, "remove", "codegraph"); code != 1 || !strings.Contains(errOut, "built-in") {
		t.Errorf("remove a built-in: exit %d %q", code, errOut)
	}
	if code, _, errOut := toolRun("", false, "remove", "../demo"); code != 1 || !strings.Contains(errOut, "not a tool name") {
		t.Errorf("remove a path: exit %d %q", code, errOut)
	}
	if code, _, errOut := toolRun("", false, "remove", "nope"); code != 1 || !strings.Contains(errOut, "no user tool") {
		t.Errorf("remove a missing tool: exit %d %q", code, errOut)
	}

	old := toolWired
	toolWired = func(string) (bool, error) { return true, nil }
	code, _, errOut := toolRun("", false, "remove", "demo")
	toolWired = old
	if code != 1 || !strings.Contains(errOut, "wired") || !exists(t, dest) {
		t.Fatalf("remove while wired: exit %d %q", code, errOut)
	}

	if code, _, errOut := toolRun("", false, "remove", "demo"); code != 0 || exists(t, dest) {
		t.Fatalf("remove: exit %d %q", code, errOut)
	}
}

// TestToolRemoveInvalid: a refused definition can still be removed.
func TestToolRemoveInvalid(t *testing.T) {
	_, dest := toolHome(t, demoTool)
	writeAt(t, dest, "not toml [", 0o600)
	if code, _, errOut := toolRun("", false, "remove", "demo"); code != 0 || exists(t, dest) {
		t.Fatalf("exit %d %q", code, errOut)
	}
}

func TestToolDelegated(t *testing.T) {
	src, dest := toolHome(t, demoTool)
	t.Setenv(borrow.Delegated, "1")
	for _, args := range [][]string{{"add", src, "--yes"}, {"remove", "demo"}} {
		if code, _, errOut := toolRun("", false, args...); code != 1 || !strings.Contains(errOut, "borrow child") {
			t.Errorf("%v: exit %d %q", args, code, errOut)
		}
	}
	if exists(t, dest) {
		t.Error("a borrow child added a tool")
	}
}

func TestToolList(t *testing.T) {
	isolate(t)
	code, out, errOut := run("tool", "list")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, w := range []string{"NAME", "codegraph", "built-in", "built-in:playwright.toml", "off"} {
		if !strings.Contains(out, w) {
			t.Errorf("list lacks %q:\n%s", w, out)
		}
	}
	// A refused user file is named, and the rest still listed.
	writeAt(t, filepath.Join(os.Getenv(home.Env), "tools", "bad.toml"), "name = 1\n", 0o600)
	code, out, errOut = run("tool", "list")
	if code != 1 || !strings.Contains(errOut, "bad.toml") || !strings.Contains(out, "codegraph") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
}

func TestToolUsage(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{{"tool"}, {"tool", "add"}, {"tool", "add", "a", "b"}, {"tool", "list", "x"}, {"tool", "remove", "x", "--yes"}, {"tool", "add", "x", "--force"}, {"tool", "frob"}} {
		if code, _, _ := run(args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
