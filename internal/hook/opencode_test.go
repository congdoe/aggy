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

// TestDeniedNameTemplates (T6967 review 7): the guard keeps .env.*, so an
// unusual name stays refused, and lets exactly the four templates through.
func TestDeniedNameTemplates(t *testing.T) {
	names := []string{".env", ".env.*", "*.pem"}
	readable := []string{".env.example", ".env.sample", ".env.template", ".env.dist"}
	for _, tc := range []struct {
		path string
		want bool
	}{
		{".env", true}, {"a/.env.vault", true}, {".env.keys", true}, {".env.local.backup", true},
		{".env.example", false}, {"x/.ENV.SAMPLE", false}, {".env.template", false}, {".env.dist", false},
		{".env.example.local", true}, {"key.pem", true}, {"main.go", false},
	} {
		if got := deniedName(tc.path, names, readable); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.path, got, tc.want)
		}
	}
}

// TestTemplateMustBePlain (T6967 re-review): a template name is exempt only
// on a regular file with one link; a symlink or a hard link to .env is
// refused like .env.
func TestTemplateMustBePlain(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("SECRET=x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".env.example"), []byte("SECRET="), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".env", filepath.Join(repo, ".env.sample")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(repo, ".env"), filepath.Join(repo, ".env.dist")); err != nil {
		t.Fatal(err)
	}
	env := OpenCodeEnv{DeniedNames: []string{".env", ".env.*"}, ReadableNames: []string{".env.example", ".env.sample", ".env.template", ".env.dist"}}
	for _, tc := range []struct {
		path string
		deny bool
	}{
		{".env.example", false}, {".env.template", false}, // a regular file, and one not there yet
		{".env.sample", true}, {".env.dist", true}, {".env", true},
	} {
		b, err := json.Marshal(OpenCodeInput{Tool: "read", Path: tc.path, Directory: repo, Root: repo})
		if err != nil {
			t.Fatal(err)
		}
		if d := OpenCodeGuard(b, env); d.Deny != tc.deny {
			t.Errorf("%s: %+v, want deny %v", tc.path, d, tc.deny)
		}
	}
}

// TestOpenCodeStrictGuard (PR #129 review, HIGH-2, and the owner decision of
// 2026-11-10): on a strict launch the guard runs a strict match only when the
// plugin's evaluate hook ran in this process and cleared this very call, by a
// prompt or an exact pre-approval: a saved Always allow with the hook API
// absent, aggy failing, timing out, or the plan removed (none of which clears)
// runs nothing strict. A private key may be read by its own name, cleared;
// through a link, by a second name, or by a harmless name that leads to one,
// it is refused.
func TestOpenCodeStrictGuard(t *testing.T) {
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("certs/server.key")
	write("id_ed25519")
	write("deploy.pem")
	if err := os.Symlink("certs/server.key", filepath.Join(repo, "notes.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("certs/server.key", filepath.Join(repo, "alias.key")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(repo, "id_ed25519"), filepath.Join(repo, "copy.pem")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("certs", filepath.Join(repo, "c")); err != nil {
		t.Fatal(err)
	}
	const force = "git push --force origin main"
	env := OpenCodeEnv{DeniedNames: []string{".env", "*.pem", "*.key", "id_ed25519"}, KeyNames: []string{"*.pem", "*.key", "id_ed25519"},
		StrictShell: func(_, _, cmd string) bool { return strings.Contains(cmd, "--force") && strings.Contains(cmd, "main") }}
	cases := []struct {
		name string
		in   OpenCodeInput
		why  string
		used int
	}{
		// The shell.
		{"cleared by a prompt", OpenCodeInput{Tool: "shell", Command: force, Strict: true, Evaluating: true, Cleared: []string{"ls", force}}, "", 2},
		{"saved allow, hook API absent", OpenCodeInput{Tool: "shell", Command: force, Strict: true, Always: true}, whyUncleared, 0},
		{"saved allow, aggy failed or timed out", OpenCodeInput{Tool: "shell", Command: force, Strict: true, Evaluating: true, Always: true}, whyUncleared, 0},
		{"another command was cleared", OpenCodeInput{Tool: "shell", Command: force + " && true", Strict: true, Evaluating: true, Cleared: []string{force}}, whyUncleared, 0},
		{"not strict on this launch", OpenCodeInput{Tool: "shell", Command: force}, "", 0},
		{"under --auto", OpenCodeInput{Tool: "shell", Command: force, Strict: true, Auto: true}, "", 0},
		{"a command no strict group matches", OpenCodeInput{Tool: "shell", Command: "go test ./...", Strict: true}, "", 0},
		// Private key reads.
		{"a key by its own name, cleared", OpenCodeInput{Tool: "read", Path: "certs/server.key", Strict: true, Evaluating: true, Cleared: []string{filepath.Join(repo, "certs", "server.key")}}, "", 1},
		{"cleared by a relative resource", OpenCodeInput{Tool: "read", Path: filepath.Join(repo, "deploy.pem"), Strict: true, Evaluating: true, Cleared: []string{"deploy.pem"}}, "", 1},
		{"a key with a second name", OpenCodeInput{Tool: "read", Path: "id_ed25519", Strict: true, Evaluating: true, Cleared: []string{"id_ed25519"}}, whyDeniedName, 0},
		{"a key, not cleared", OpenCodeInput{Tool: "read", Path: "certs/server.key", Strict: true, Evaluating: true}, whyUncleared, 0},
		{"a key, no evaluate hook", OpenCodeInput{Tool: "read", Path: "certs/server.key", Strict: true, Cleared: []string{"certs/server.key"}}, whyUncleared, 0},
		{"a key, not strict", OpenCodeInput{Tool: "read", Path: "certs/server.key", Evaluating: true, Cleared: []string{"certs/server.key"}}, whyDeniedName, 0},
		{"a key under --auto", OpenCodeInput{Tool: "read", Path: "certs/server.key", Strict: true, Auto: true, Evaluating: true, Cleared: []string{"certs/server.key"}}, whyDeniedName, 0},
		{"a key, edited", OpenCodeInput{Tool: "edit", Path: "certs/server.key", Strict: true, Evaluating: true, Cleared: []string{"certs/server.key"}}, whyDeniedName, 0},
		{"a key name on a link", OpenCodeInput{Tool: "read", Path: "alias.key", Strict: true, Evaluating: true, Cleared: []string{"alias.key"}}, whyDeniedName, 0},
		{"a key through a linked folder", OpenCodeInput{Tool: "read", Path: "c/server.key", Strict: true, Evaluating: true, Cleared: []string{"c/server.key"}}, whyDeniedName, 0},
		{"a second hard link", OpenCodeInput{Tool: "read", Path: "copy.pem", Strict: true, Evaluating: true, Cleared: []string{"copy.pem"}}, whyDeniedName, 0},
		{"a harmless name for a key", OpenCodeInput{Tool: "read", Path: "notes.txt", Strict: true, Evaluating: true, Cleared: []string{"notes.txt"}}, whyDisguised, 0},
		{"a harmless name for a key, not strict", OpenCodeInput{Tool: "read", Path: "notes.txt"}, whyDisguised, 0},
		{".env stays refused", OpenCodeInput{Tool: "read", Path: ".env", Strict: true, Evaluating: true, Cleared: []string{".env"}}, whyDeniedName, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			in.Root, in.Directory = repo, repo
			b, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			d := OpenCodeGuard(b, env)
			if d.Deny != (c.why != "") || d.Reason != c.why || d.Cleared != c.used {
				t.Fatalf("got %+v, want %q, clear %d", d, c.why, c.used)
			}
		})
	}
}

// TestOpenCodeOutsideRead (T6979): on a strict launch (trusted, not --auto,
// aggy's evaluate hook there) the read tool opens an honest path outside the
// repo once a prompt cleared it. A path through a link, one that reads as
// inside but leads out, aggy's own state, a vendor credential, an edit, an
// uncleared read, --auto, and an untrusted launch are refused.
func TestOpenCodeOutsideRead(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	repo, other := filepath.Join(home, "repo"), filepath.Join(home, "other")
	state := filepath.Join(home, ".aggy")
	for _, d := range []string{repo, other, state, filepath.Join(home, ".codex")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(other, "notes.txt"), filepath.Join(other, ".env"), filepath.Join(state, "permissions.json"), filepath.Join(home, ".codex", "auth.json")} {
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(other, filepath.Join(repo, "inside")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(home, "via")); err != nil {
		t.Fatal(err)
	}
	notes := filepath.Join(other, "notes.txt")
	// #133 review, owner decision: a private key outside the repo asks, each
	// time, and runs only once a prompt cleared that very file.
	sshKey := filepath.Join(home, ".ssh", "id_ed25519")
	if err := os.MkdirAll(filepath.Dir(sshKey), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sshKey, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sshKey, filepath.Join(other, "deploy_notes")); err != nil {
		t.Fatal(err)
	}
	env := OpenCodeEnv{Getenv: os.Getenv, DeniedNames: []string{".env", "*.pem", "*.key", "id_ed25519"}, KeyNames: []string{"*.pem", "*.key", "id_ed25519"}, Protected: []string{state}}
	cases := []struct {
		name string
		in   OpenCodeInput
		why  string
		used int
	}{
		{"cleared by a prompt", OpenCodeInput{Tool: "read", Path: notes, Strict: true, Evaluating: true, Cleared: []string{"x", notes}}, "", 2},
		{"cleared, by a relative path", OpenCodeInput{Tool: "read", Path: "../other/notes.txt", Strict: true, Evaluating: true, Cleared: []string{notes}}, "", 1},
		{"cleared, by ~", OpenCodeInput{Tool: "read", Path: "~/other/notes.txt", Strict: true, Evaluating: true, Cleared: []string{"~/other/notes.txt"}}, "", 1},
		{"not cleared", OpenCodeInput{Tool: "read", Path: notes, Strict: true, Evaluating: true, Cleared: []string{filepath.Join(other, "else.txt")}}, whyNotAsked, 0},
		{"no evaluate hook", OpenCodeInput{Tool: "read", Path: notes, Strict: true, Cleared: []string{notes}}, whyNotAsked, 0},
		{"under --auto", OpenCodeInput{Tool: "read", Path: notes, Strict: true, Auto: true, Evaluating: true, Cleared: []string{notes}}, whyOutside, 0},
		{"untrusted (not strict)", OpenCodeInput{Tool: "read", Path: notes, Evaluating: true, Cleared: []string{notes}}, whyOutside, 0},
		{"an edit", OpenCodeInput{Tool: "edit", Path: notes, Strict: true, Evaluating: true, Cleared: []string{notes}}, whyOutside, 0},
		{"an inside name that leads out", OpenCodeInput{Tool: "read", Path: "inside/notes.txt", Strict: true, Evaluating: true, Cleared: []string{"inside/notes.txt", notes}}, whyOutside, 0},
		{"through a link outside", OpenCodeInput{Tool: "read", Path: filepath.Join(home, "via", "notes.txt"), Strict: true, Evaluating: true, Cleared: []string{filepath.Join(home, "via", "notes.txt")}}, whyOutside, 0},
		{"aggy's own state", OpenCodeInput{Tool: "read", Path: filepath.Join(state, "permissions.json"), Strict: true, Evaluating: true, Cleared: []string{filepath.Join(state, "permissions.json")}}, whyOutside, 0},
		// T6931 (#138 review M3): a stored key's file, cleared or not.
		{"a stored key's file", OpenCodeInput{Tool: "read", Path: filepath.Join(state, "run", "p.1.memory", "opencode", "keys", "anthropic"), Strict: true, Evaluating: true, Cleared: []string{filepath.Join(state, "run", "p.1.memory", "opencode", "keys", "anthropic")}}, whyOutside, 0},
		{"a vendor credential", OpenCodeInput{Tool: "read", Path: filepath.Join(home, ".codex", "auth.json"), Strict: true, Evaluating: true, Cleared: []string{filepath.Join(home, ".codex", "auth.json")}}, whyOutside, 0},
		{".env outside stays refused", OpenCodeInput{Tool: "read", Path: filepath.Join(other, ".env"), Strict: true, Evaluating: true, Cleared: []string{filepath.Join(other, ".env")}}, whyDeniedName, 0},
		{"a private key outside, cleared", OpenCodeInput{Tool: "read", Path: "~/.ssh/id_ed25519", Strict: true, Evaluating: true, Cleared: []string{sshKey}}, "", 1},
		{"a private key outside, not cleared", OpenCodeInput{Tool: "read", Path: sshKey, Strict: true, Evaluating: true, Cleared: []string{notes}}, whyNotAsked, 0},
		{"a private key outside, saved allow and no hook", OpenCodeInput{Tool: "read", Path: sshKey, Strict: true, Always: true, Cleared: []string{sshKey}}, whyNotAsked, 0},
		{"a private key outside, under --auto", OpenCodeInput{Tool: "read", Path: sshKey, Strict: true, Auto: true, Evaluating: true, Cleared: []string{sshKey}}, whyOutside, 0},
		{"a private key behind a harmless name", OpenCodeInput{Tool: "read", Path: filepath.Join(other, "deploy_notes"), Strict: true, Evaluating: true, Cleared: []string{filepath.Join(other, "deploy_notes")}}, whyOutside, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := c.in
			in.Root, in.Directory = repo, repo
			b, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			d := OpenCodeGuard(b, env)
			if d.Deny != (c.why != "") || d.Reason != c.why || d.Cleared != c.used {
				t.Fatalf("got %+v, want %q, clear %d", d, c.why, c.used)
			}
		})
	}
}

// TestOutsideProtectedSpelling (#133 review): aggy's state folder spelled in
// other case, or in another Unicode normalisation, is still aggy's folder on
// a file system that folds them (macOS by default), so a cleared outside read
// of it is refused. Skipped where the file system tells the spellings apart.
func TestOutsideProtectedSpelling(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	repo := filepath.Join(home, "repo")
	state := filepath.Join(home, ".aggy")
	nfc := filepath.Join(home, "café")
	for _, d := range []string{repo, state, nfc} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(state, "trust.json"), filepath.Join(nfc, "trust.json")} {
		if err := os.WriteFile(f, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	env := OpenCodeEnv{Getenv: os.Getenv, Protected: []string{state, nfc}}
	for _, c := range []struct{ name, path, probe string }{
		{"other case", filepath.Join(home, ".AGGY", "trust.json"), filepath.Join(home, ".AGGY")},
		{"another normalisation", filepath.Join(home, "café", "trust.json"), filepath.Join(home, "café")},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := os.Lstat(c.probe); err != nil {
				t.Skipf("this file system tells the spellings apart (%v)", err)
			}
			in := OpenCodeInput{Tool: "read", Path: c.path, Root: repo, Directory: repo, Strict: true, Evaluating: true, Cleared: []string{c.path}}
			b, err := json.Marshal(in)
			if err != nil {
				t.Fatal(err)
			}
			if d := OpenCodeGuard(b, env); !d.Deny || d.Reason != whyOutside {
				t.Fatalf("got %+v, want refused", d)
			}
			if !SameAsProtected(c.path, env.Protected, os.Stat, os.Lstat) {
				t.Error("not the same folder by identity")
			}
		})
	}
}

// TestHoldsRuleCollapsesBoth: two spaces in a rule must not hide it from the gate.
func TestHoldsRuleCollapsesBoth(t *testing.T) {
	for _, tc := range []struct {
		text, rule string
		want       bool
	}{
		{"PATH=./bin; mytool  x", "mytool  x", true},
		{"PATH=./bin; mytool x", "mytool  x", true},
		{"PATH=./bin; mytool  x", "mytool x", true},
		{"PATH=./bin; other", "mytool  x", false},
		{"anything", "   ", false},
	} {
		if got := holdsRule(tc.text, []string{tc.rule}); got != tc.want {
			t.Errorf("%q, %q: %v", tc.text, tc.rule, got)
		}
	}
}
