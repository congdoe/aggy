package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
)

// entry is one path in a tree snapshot.
type entry struct {
	mode fs.FileMode
	data string // file bytes, or a symlink's target
}

// snapshot records every path under root with its type, permission bits,
// and content. Tests may use os directly.
func snapshot(t *testing.T, root string) map[string]entry {
	t.Helper()
	out := map[string]entry{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		e := entry{mode: fi.Mode()}
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			e.data, err = os.Readlink(p)
		case fi.Mode().IsRegular():
			var b []byte
			b, err = os.ReadFile(p)
			e.data = string(b)
		}
		rel, _ := filepath.Rel(root, p)
		out[rel] = e
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// diffSnapshots lists every difference between two snapshots.
func diffSnapshots(a, b map[string]entry) []string {
	var out []string
	for p, ea := range a {
		eb, ok := b[p]
		switch {
		case !ok:
			out = append(out, "missing: "+p)
		case ea.mode != eb.mode:
			out = append(out, fmt.Sprintf("mode %s: %v -> %v", p, ea.mode, eb.mode))
		case ea.data != eb.data:
			out = append(out, fmt.Sprintf("content %s: %q -> %q", p, ea.data, eb.data))
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			out = append(out, "extra: "+p)
		}
	}
	sort.Strings(out)
	return out
}

func writeAt(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // past the umask
		t.Fatal(err)
	}
}

// addJSONKey is a setup-style patch on a JSON settings file: it adds a
// key before the closing brace, as a minimal text edit.
func addJSONKey(b []byte) ([]byte, error) {
	const kv = `"aggyParity": true`
	if bytes.Contains(b, []byte(kv)) {
		return b, nil
	}
	i := bytes.LastIndexByte(b, '}')
	if i < 0 {
		return nil, fmt.Errorf("no object")
	}
	out := append([]byte{}, bytes.TrimRight(b[:i], " \t\r\n")...)
	out = append(out, ",\n  "+kv+"\n"...)
	return append(out, b[i:]...), nil
}

// TestM0Gate is the M0 exit test: with realistic vendor config files in a
// temp HOME, setup-style patches applied through the manifest over
// several runs, then aggy unwire --all leaves every file byte-identical
// with the same mode, and no extra file or directory behind.
func TestM0Gate(t *testing.T) {
	h := isolate(t)
	src := filepath.Join(h, "src")

	// Vendor config as people have it: comments, CRLF, tables, no
	// trailing newline, odd modes. A fake credential sits next to them
	// and must come through untouched.
	codexCfg := filepath.Join(h, ".codex", "config.toml")
	writeAt(t, codexCfg, "# Codex\r\n# hand edited\r\n\r\nmodel = \"gpt-5\"\r\nproject_doc_fallback_filenames = [\r\n  \"AGENTS.override.md\", # team\r\n]\r\n\r\n[mcp_servers.docs]\r\ncommand = \"npx\"\r\n", 0o640)
	writeAt(t, filepath.Join(h, ".codex", "auth.json"), `{"token":"fake"}`, 0o600)
	claudeSettings := filepath.Join(h, ".claude", "settings.json")
	writeAt(t, claudeSettings, "{\n  \"permissions\": {\"deny\": [\"Read(.env)\"]},\n  \"model\": \"opus\"\n}", 0o600)
	writeAt(t, filepath.Join(h, ".claude", ".credentials.json"), `{"fake":true}`, 0o600)
	writeAt(t, filepath.Join(h, ".claude", "CLAUDE.md"), "# global\n", 0o644)
	// Grok has no config yet: setup creates a file in new directories.
	grokCfg := filepath.Join(h, ".grok", "aggy", "settings.toml")

	// A repo with no info/exclude, a repo with git's template exclude in
	// CRLF and no trailing newline, and a linked worktree of the first.
	repo := filepath.Join(src, "app")
	writeAt(t, filepath.Join(repo, "CLAUDE.md"), "# app\n", 0o644)
	if err := os.MkdirAll(filepath.Join(repo, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	wtGit := filepath.Join(repo, ".git", "worktrees", "wt")
	writeAt(t, filepath.Join(wtGit, "commondir"), "../..\n", 0o644)
	wt := filepath.Join(src, "app-wt")
	writeAt(t, filepath.Join(wt, ".git"), "gitdir: "+wtGit+"\n", 0o644)
	lib := filepath.Join(src, "lib")
	writeAt(t, filepath.Join(lib, ".git", "info", "exclude"), "# git ls-files --others --exclude-from=.git/info/exclude\r\n*.o", 0o644)

	before := snapshot(t, h)

	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Open(s)
	if err != nil {
		t.Fatal(err)
	}
	build := func() []patch.Patch {
		t.Helper()
		var ps []patch.Patch
		add := func(p patch.Patch, err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
			ps = append(ps, p)
		}
		codexPath, err := parity.CodexConfigPath()
		if err != nil || codexPath != codexCfg {
			t.Fatalf("CodexConfigPath = %s, %v", codexPath, err)
		}
		add(parity.CodexFallbackPatch(codexPath))
		add(parity.GitExcludePatch(repo))
		add(parity.GitExcludePatch(wt)) // the same exclude file as repo
		add(parity.GitExcludePatch(lib))
		hc, err := patch.HashFile(claudeSettings)
		add(patch.Patch{File: claudeSettings, ExpectedHash: hc, Transform: addJSONKey}, err)
		hg, err := patch.HashFile(grokCfg)
		add(patch.Patch{File: grokCfg, ExpectedHash: hg, Transform: appendOnce("aggy = true\n")}, err)
		return ps
	}
	// Run 1: setup. Run 2: a later change to a file run 1 changed. Run 3:
	// setup again, which finds everything in place.
	if _, err := m.Run(ctx, manifest.Write, manifest.NewRunID(), build(), io.Discard); err != nil {
		t.Fatal(err)
	}
	hc, err := patch.HashFile(codexCfg)
	if err != nil {
		t.Fatal(err)
	}
	later := []patch.Patch{{File: codexCfg, ExpectedHash: hc, Transform: appendOnce("# wired by aggy\r\n")}}
	if _, err := m.Run(ctx, manifest.Write, manifest.NewRunID(), later, io.Discard); err != nil {
		t.Fatal(err)
	}
	cs, err := m.Run(ctx, manifest.Write, manifest.NewRunID(), build(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if c.Status != store.ChangeNoop {
			t.Errorf("setup again changed %s", c.File)
		}
	}
	_ = s.Close()

	wired := snapshot(t, h)
	changed := diffSnapshots(before, wired)
	for _, want := range []string{
		"content .codex/config.toml", "content .claude/settings.json",
		"extra: .grok", "extra: src/app/.git/info", "content src/lib/.git/info/exclude",
	} {
		if !strings.Contains(strings.Join(changed, "\n"), want) {
			t.Errorf("setup did not make the change %q; changes:\n%s", want, strings.Join(changed, "\n"))
		}
	}
	if b, _ := os.ReadFile(codexCfg); !bytes.Contains(b, []byte(`"AGENTS.override.md", "CLAUDE.md",`)) {
		t.Errorf("codex config = %q", b)
	}

	// A dry run changes nothing.
	if code, _, errOut := run("unwire", "--all", "--dry-run"); code != 0 {
		t.Fatalf("dry run: exit %d: %s", code, errOut)
	}
	if d := diffSnapshots(wired, snapshot(t, h)); len(d) > 0 {
		t.Errorf("the dry run changed files:\n%s", strings.Join(d, "\n"))
	}

	code, out, errOut := run("unwire", "--all")
	if code != 0 {
		t.Fatalf("unwire: exit %d\n%s\n%s", code, out, errOut)
	}
	if d := diffSnapshots(before, snapshot(t, h)); len(d) > 0 {
		t.Errorf("unwire --all did not restore HOME exactly:\n%s", strings.Join(d, "\n"))
	}
	if code, out, _ := run("unwire", "--all"); code != 0 || !strings.Contains(out, "Nothing to undo") {
		t.Errorf("second unwire: exit %d, %q", code, out)
	}
}
