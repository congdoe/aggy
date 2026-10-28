package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
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
//
// It also covers the rows that are not plain applied changes: a failed
// create that made its directories, a pending change whose write
// happened before the process stopped, a file whose mode a tool changed
// after aggy wrote it, a file whose mode is not the default under a
// strict umask, and the link and removal kinds: a symlink made in new
// directories, a symlink replacing another, and a file removed. Skipping pending or failed rows, or ignoring the
// recorded mode, fails this test.
func TestM0Gate(t *testing.T) {
	h := isolate(t)
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	src := filepath.Join(h, "src")

	// Vendor config as people have it: comments, CRLF, tables, no
	// trailing newline, odd modes. A fake credential sits next to them
	// and must come through untouched.
	codexCfg := filepath.Join(h, ".codex", "config.toml")
	writeAt(t, codexCfg, "# Codex\r\n# hand edited\r\n\r\nmodel = \"gpt-5\"\r\nproject_doc_fallback_filenames = [\r\n  \"AGENTS.override.md\", # team\r\n]\r\n\r\n[mcp_servers.docs]\r\ncommand = \"npx\"\r\n", 0o640)
	writeAt(t, filepath.Join(h, ".codex", "auth.json"), `{"token":"fake"}`, 0o600)
	claudeSettings := filepath.Join(h, ".claude", "settings.json")
	writeAt(t, claudeSettings, "{\n  \"permissions\": {\"deny\": [\"Read(.env)\"]},\n  \"model\": \"opus\"\n}", 0o644)
	writeAt(t, filepath.Join(h, ".claude", ".credentials.json"), `{"fake":true}`, 0o600)
	claudeMD := filepath.Join(h, ".claude", "CLAUDE.md")
	writeAt(t, claudeMD, "# global\n", 0o644)
	// Grok has no config yet: setup creates a file in new directories.
	grokCfg := filepath.Join(h, ".grok", "aggy", "settings.toml")
	// Skill links: one made in new directories, one replacing an old
	// link, and a stale file removed.
	skillDir := filepath.Join(h, "skills-store", "aggy")
	writeAt(t, filepath.Join(skillDir, "SKILL.md"), "---\nname: aggy\n---\n", 0o644)
	claudeLink := filepath.Join(h, ".claude", "skills", "aggy")
	agentsLink := filepath.Join(h, ".agents", "skills", "aggy")
	if err := os.MkdirAll(filepath.Dir(agentsLink), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../old-skill", agentsLink); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(h, ".agents", "stale.md")
	writeAt(t, stale, "stale\r\n", 0o640)

	// A repo with no info/exclude, a repo with git's template exclude in
	// CRLF and no trailing newline, and a linked worktree of the first.
	repo := filepath.Join(src, "app")
	writeAt(t, filepath.Join(repo, "CLAUDE.md"), "# app\n", 0o644)
	if err := os.MkdirAll(filepath.Join(repo, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeAt(t, filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
	wtGit := filepath.Join(repo, ".git", "worktrees", "wt")
	writeAt(t, filepath.Join(wtGit, "commondir"), "../..\n", 0o644)
	writeAt(t, filepath.Join(wtGit, "HEAD"), "ref: refs/heads/wt\n", 0o644)
	wt := filepath.Join(src, "app-wt")
	writeAt(t, filepath.Join(wt, ".git"), "gitdir: "+wtGit+"\n", 0o644)
	lib := filepath.Join(src, "lib")
	writeAt(t, filepath.Join(lib, ".git", "info", "exclude"), "# git ls-files --others --exclude-from=.git/info/exclude\r\n*.o", 0o644)
	writeAt(t, filepath.Join(lib, ".git", "HEAD"), "ref: refs/heads/main\n", 0o644)
	if err := os.MkdirAll(filepath.Join(lib, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}

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
		add(patch.Patch{File: grokCfg, ExpectedHash: hg, Transform: appendOnce("aggy = true\n"), Mode: 0o640}, err)
		for _, l := range []string{claudeLink, agentsLink} {
			st, err := patch.ReadState(l)
			add(patch.Patch{File: l, ExpectedHash: st.Hash, Link: skillDir}, err)
		}
		st, err := patch.ReadState(stale)
		add(patch.Patch{File: stale, ExpectedHash: st.Hash, Remove: true}, err)
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
	if fi, err := os.Stat(grokCfg); err != nil || fi.Mode().Perm() != 0o640 {
		t.Errorf("created grok config mode = %v, %v, want 0640", fi, err)
	}
	// Run 4 fails after making the directories for a new file.
	failedFile := filepath.Join(h, ".config", "aggy", "failed.toml")
	restore := patch.SetTestHookBeforeWrite(func(f string) error {
		if f == failedFile {
			return fmt.Errorf("injected failure")
		}
		return nil
	})
	_, err = m.Run(ctx, manifest.Write, manifest.NewRunID(), []patch.Patch{{File: failedFile, ExpectedHash: patch.NoFile, Transform: appendOnce("x = 1\n")}}, io.Discard)
	restore()
	if err == nil {
		t.Fatal("the injected failure did not fail the run")
	}
	if _, err := os.Stat(filepath.Dir(failedFile)); err != nil {
		t.Fatalf("the failed run made no directories: %v", err)
	}
	// Run 5 writes, and the process stops before recording it: the row
	// stays pending with no backup path.
	hm, err := patch.HashFile(claudeMD)
	if err != nil {
		t.Fatal(err)
	}
	pend, err := m.Run(ctx, manifest.Write, manifest.NewRunID(), []patch.Patch{{File: claudeMD, ExpectedHash: hm, Transform: appendOnce("- use aggy\n")}}, io.Discard)
	if err != nil || len(pend) != 1 {
		t.Fatalf("run 5 = %+v, %v", pend, err)
	}
	if _, err := s.DB().ExecContext(ctx, `UPDATE changes SET status = 'pending', backup_path = '' WHERE change_id = ?`, pend[0].ChangeID); err != nil {
		t.Fatal(err)
	}
	var statuses []string
	rows, _ := s.Changes(ctx, "")
	for _, c := range rows {
		statuses = append(statuses, c.Status)
	}
	for _, want := range []string{store.ChangeFailed, store.ChangePending} {
		if !slices.Contains(statuses, want) {
			t.Errorf("no %s row in the manifest: %v", want, statuses)
		}
	}
	_ = s.Close()
	// Another tool tightens a file's mode after aggy wrote it; the
	// content is still aggy's, so unwire restores the recorded mode.
	if err := os.Chmod(codexCfg, 0o600); err != nil {
		t.Fatal(err)
	}

	wired := snapshot(t, h)
	changed := diffSnapshots(before, wired)
	for _, want := range []string{
		"content .claude/settings.json",
		"extra: .grok", "extra: src/app/.git/info", "content src/lib/.git/info/exclude",
		"extra: .config", "content .claude/CLAUDE.md", "mode .codex/config.toml",
		"extra: .claude/skills", "content .agents/skills/aggy", "missing: .agents/stale.md",
	} {
		if !strings.Contains(strings.Join(changed, "\n"), want) {
			t.Errorf("setup did not make the change %q; changes:\n%s", want, strings.Join(changed, "\n"))
		}
	}
	if b, _ := os.ReadFile(codexCfg); !bytes.Contains(b, []byte(`"AGENTS.override.md", "CLAUDE.md",`)) {
		t.Errorf("codex config = %q", b)
	}

	// A dry run shows each undo and changes nothing.
	code, dry, errOut := run("unwire", "--all", "--dry-run")
	if code != 0 {
		t.Fatalf("dry run: exit %d: %s", code, errOut)
	}
	for _, want := range []string{
		"--- " + codexCfg + "\n+++ " + codexCfg,
		`-# wired by aggy\r`,
		`-  "AGENTS.override.md", "CLAUDE.md", # team\r`,
		`+  "AGENTS.override.md", # team\r`,
		"--- " + grokCfg + "\n+++ /dev/null",
		"-aggy = true",
		`-  "aggyParity": true`,
		"-.aggy/",
		"-- use aggy",
		"# " + codexCfg + ": mode 0600 -> 0640",
		"# " + filepath.Join(h, ".config") + ": would remove the directory if empty",
		"# " + claudeLink + ": symlink to " + skillDir + " -> nothing",
		"# " + filepath.Join(h, ".claude", "skills") + ": would remove the directory if empty",
		"# " + agentsLink + ": symlink to " + skillDir + " -> symlink to ../../old-skill",
		"# " + stale + ": nothing -> file (7 bytes,",
	} {
		if !strings.Contains(dry, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, dry)
		}
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
