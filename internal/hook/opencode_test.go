package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenCodeGuard runs the guard on real files: a repo, a link out of
// it, and a folder beside it.
func TestOpenCodeGuard(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	other := filepath.Join(base, "other")
	data := filepath.Join(base, "data")
	for _, d := range []string{filepath.Join(repo, "sub"), other, filepath.Join(data, "opencode", "tool-output")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(other, "key.txt")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(repo, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(repo, "linkdir")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "ok.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(data, "opencode", "tool-output", "out_1")
	if err := os.WriteFile(saved, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := OpenCodeEnv{DeniedNames: []string{".env", "*.pem"}, ShellRules: []string{"git status", "ls"}, Getenv: func(k string) string {
		if k == "XDG_DATA_HOME" {
			return data
		}
		return ""
	}}
	cases := []struct {
		name string
		in   OpenCodeInput
		why  string
	}{
		{"a read inside", OpenCodeInput{Tool: "read", Path: "ok.txt"}, ""},
		{"an absolute read inside", OpenCodeInput{Tool: "read", Path: filepath.Join(repo, "ok.txt")}, ""},
		{"a new file inside", OpenCodeInput{Tool: "write", Path: "sub/new/file.go"}, ""},
		{"a read out by path", OpenCodeInput{Tool: "read", Path: secret}, whyOutside},
		{"a read through a repo symlink", OpenCodeInput{Tool: "read", Path: "notes.txt"}, whyOutside},
		{"a read through a linked folder", OpenCodeInput{Tool: "read", Path: "linkdir/key.txt"}, whyOutside},
		{"a write through a linked folder to a new file", OpenCodeInput{Tool: "write", Path: "linkdir/new.txt"}, whyOutside},
		{"dot dot out", OpenCodeInput{Tool: "edit", Path: "../other/key.txt"}, whyOutside},
		{"dot dot staying in", OpenCodeInput{Tool: "edit", Path: "sub/../ok.txt"}, ""},
		{"grep out", OpenCodeInput{Tool: "grep", Path: other}, whyOutside},
		{"glob with no path", OpenCodeInput{Tool: "glob"}, ""},
		{"a shell workdir out", OpenCodeInput{Tool: "shell", Command: "ls", Workdir: other}, whyOutside},
		{"a shell workdir in", OpenCodeInput{Tool: "shell", Command: "ls", Workdir: "sub"}, ""},
		{"an ordinary command", OpenCodeInput{Tool: "shell", Command: "go test ./..."}, ""},
		{"a declaration", OpenCodeInput{Tool: "shell", Command: "PATH=/x:$PATH; git status"}, whyVariable("PATH")},
		{"a function by a rule's name", OpenCodeInput{Tool: "shell", Command: "git() { :; }; git status"}, shellScan("git() { :; }; git status", []string{"git status", "ls"}, false)},
		// Full auto: a declaration hides nothing there, and the rest still holds.
		{"full auto skips the scan", OpenCodeInput{Tool: "shell", Auto: true, Command: "PATH=/x:$PATH; git status"}, ""},
		{"full auto skips the function check", OpenCodeInput{Tool: "shell", Auto: true, Command: "git() { :; }; git status"}, ""},
		{"full auto keeps auth export", OpenCodeInput{Tool: "shell", Auto: true, Command: "opencode auth export"}, whyAuthExport},
		{"full auto keeps the workdir", OpenCodeInput{Tool: "shell", Auto: true, Command: "ls", Workdir: other}, whyOutside},
		{"full auto keeps the paths", OpenCodeInput{Tool: "read", Auto: true, Path: secret}, whyOutside},
		{"full auto keeps the names", OpenCodeInput{Tool: "read", Auto: true, Path: ".env"}, whyDeniedName},
		{"full auto keeps the folder", OpenCodeInput{Tool: "shell", Auto: true, Command: "ls", Directory: other}, whyFolder},
		{"auth export", OpenCodeInput{Tool: "shell", Command: "opencode auth export"}, whyAuthExport},
		{"auth export with a flag", OpenCodeInput{Tool: "shell", Command: "opencode --standalone auth   export --format json"}, whyAuthExport},
		{"auth export in a chain", OpenCodeInput{Tool: "shell", Command: "true && /opt/bin/opencode auth export"}, whyAuthExport},
		{"auth list is fine", OpenCodeInput{Tool: "shell", Command: "opencode auth list"}, ""},
		{"a patch inside", OpenCodeInput{Tool: "patch", PatchText: "*** Begin Patch\n*** Update File: ok.txt\n@@\n-a\n+b\n*** End Patch"}, ""},
		{"a patch out", OpenCodeInput{Tool: "patch", PatchText: "*** Begin Patch\n*** Add File: ../other/x\n+b\n*** End Patch"}, whyOutside},
		{"a patch through a link", OpenCodeInput{Tool: "patch", PatchText: "*** Update File: notes.txt\n"}, whyOutside},
		{"its own saved output", OpenCodeInput{Tool: "read", Path: saved}, ""},
		{"not by write", OpenCodeInput{Tool: "write", Path: saved}, whyOutside},
		{"not the data folder", OpenCodeInput{Tool: "read", Path: filepath.Join(data, "opencode", "opencode.db")}, whyOutside},
		{"from another folder", OpenCodeInput{Tool: "shell", Command: "ls", Directory: other}, whyFolder},
		{"a tool it does not know", OpenCodeInput{Tool: "websearch"}, ""},
		// H3: OpenCode expands these itself (file-access.ts).
		{"~ alone", OpenCodeInput{Tool: "read", Path: "~"}, whyOutside},
		{"~/", OpenCodeInput{Tool: "read", Path: "~/.ssh/id_rsa"}, whyOutside},
		{"~/ in edit", OpenCodeInput{Tool: "edit", Path: "~/.bashrc"}, whyOutside},
		{"~/ in write", OpenCodeInput{Tool: "write", Path: "~/.bashrc"}, whyOutside},
		{"~/ in glob", OpenCodeInput{Tool: "glob", Path: "~"}, whyOutside},
		{"~/ in grep", OpenCodeInput{Tool: "grep", Path: "~/.ssh"}, whyOutside},
		{"~/ as a workdir", OpenCodeInput{Tool: "shell", Command: "ls", Workdir: "~"}, whyOutside},
		{"~/ deeper as a workdir", OpenCodeInput{Tool: "shell", Command: "ls", Workdir: "~/.ssh"}, whyOutside},
		{"~user", OpenCodeInput{Tool: "read", Path: "~root/x"}, whyOutside},
		{"~/ in a patch", OpenCodeInput{Tool: "patch", PatchText: "*** Begin Patch\n*** Add File: ~/.bashrc\n+x\n*** End Patch"}, whyOutside},
		{"a patch header indented", OpenCodeInput{Tool: "patch", PatchText: "*** Begin Patch\n  *** Add File: ../outside\n+x\n*** End Patch"}, whyOutside},
		{"a patch with CRLF", OpenCodeInput{Tool: "patch", PatchText: "*** Begin Patch\r\n*** Add File: ../outside\r\n+x\r\n*** End Patch"}, whyOutside},
		{"a patch header with two spaces", OpenCodeInput{Tool: "patch", PatchText: "***  Add File: ../outside\n"}, whyOutside},
		{"a glob pattern with ~", OpenCodeInput{Tool: "glob", Pattern: "~/.ssh/*"}, whyOutside},
		{"a glob pattern absolute", OpenCodeInput{Tool: "glob", Pattern: "/etc/*"}, whyOutside},
		{"a glob pattern with dot dot", OpenCodeInput{Tool: "glob", Pattern: "../*"}, whyOutside},
		{"a glob pattern with dot dot in braces", OpenCodeInput{Tool: "glob", Pattern: "{a,../b}/*"}, whyOutside},
		{"a glob pattern inside", OpenCodeInput{Tool: "glob", Pattern: "**/*.go"}, ""},
		{"a path on a tool it does not know", OpenCodeInput{Tool: "list", Path: other}, whyOutside},
		// L1: a policy's rule matches case exactly.
		{"a denied name", OpenCodeInput{Tool: "read", Path: ".env"}, whyDeniedName},
		{"a denied name in capitals", OpenCodeInput{Tool: "read", Path: ".ENV"}, whyDeniedName},
		{"a denied name in grep", OpenCodeInput{Tool: "grep", Path: "sub/Server.PEM"}, whyDeniedName},
		{"a name like it", OpenCodeInput{Tool: "read", Path: "env.go"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			in.Root = repo
			if in.Directory == "" {
				in.Directory = repo
			}
			b, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			d := OpenCodeGuard(b, env)
			if d.Deny != (c.why != "") || d.Reason != c.why {
				t.Fatalf("got %+v, want %q", d, c.why)
			}
		})
	}
}

func TestOpenCodeGuardFailsClosed(t *testing.T) {
	for _, p := range []string{"", "not json", `{"tool":"read"}`, `{"tool":"read","directory":"rel","root":"/r"}`, `{"directory":"/r","root":"/r"}`} {
		if d := OpenCodeGuard([]byte(p), OpenCodeEnv{}); !d.Deny || !strings.Contains(d.Reason, "refused") {
			t.Errorf("%q: %+v", p, d)
		}
	}
	missing := filepath.Join(t.TempDir(), "gone")
	b, _ := json.Marshal(OpenCodeInput{Tool: "read", Path: "x", Directory: missing, Root: missing})
	if d := OpenCodeGuard(b, OpenCodeEnv{}); !d.Deny {
		t.Errorf("a root that does not resolve: %+v", d)
	}
}
