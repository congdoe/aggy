package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/transcript"
)

// inRepo moves the test into a fresh repo.
func inRepo(t *testing.T) string {
	t.Helper()
	repo := tempRepo(t)
	t.Chdir(repo)
	return repo
}

func aggyFileText(t *testing.T) string {
	t.Helper()
	path, err := permissionsFile()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func TestPermissionsAndConfigListOptions(t *testing.T) {
	isolate(t)
	code, out, _ := run("permissions")
	if code != 0 || !strings.Contains(out, "suggest") || !strings.Contains(out, "add <allow|ask|deny>") {
		t.Errorf("permissions: %d %q", code, out)
	}
	code, out, _ = run("config")
	if code != 0 || !strings.Contains(out, "permission-mode") || !strings.Contains(out, "default-harness") {
		t.Errorf("config: %d %q", code, out)
	}
	for _, args := range [][]string{{"permissions", "nope"}, {"config", "nope"}, {"permissions", "add"}, {"permissions", "add", "maybe", "Bash(ls)"}, {"config", "set", "x"}} {
		code, _, errb := run(args...)
		if code != 2 || !strings.Contains(errb, "[cli.usage · run ") {
			t.Errorf("%q: %d %q", args, code, errb)
		}
	}
}

func TestPermissionsAddRemove(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	key, err := safefs.CanonicalPath(repo)
	if err != nil {
		t.Fatal(err)
	}

	// A dry run has no side effects, and needs no keyboard.
	code, out, errb := run("permissions", "add", "allow", "Bash(git log -5)", "--dry-run")
	if code != 0 || !strings.Contains(out, "git log -5") {
		t.Errorf("dry run: %d %q %q", code, out, errb)
	}
	if aggyFileText(t) != "" {
		t.Error("the dry run wrote the file")
	}

	// A model's shell has no terminal: refused, tagged, nothing changed.
	code, _, errb = run("permissions", "add", "allow", "Bash(git log -5)")
	if code != 1 || !strings.Contains(errb, "[permissions.not_at_keyboard · run ") {
		t.Errorf("no terminal: %d %q", code, errb)
	}
	if aggyFileText(t) != "" {
		t.Error("a refused add wrote the file")
	}

	atKeyboard(t, "y\n")
	if code, out, errb = run("permissions", "add", "allow", "Bash(git log -5)"); code != 0 {
		t.Fatalf("add: %d %q %q", code, out, errb)
	}
	if !strings.Contains(out, "next launch or switch") || !strings.Contains(out, "aggy unwire") {
		t.Errorf("does not say when, or how to undo: %q", out)
	}
	if !strings.Contains(aggyFileText(t), key) {
		t.Errorf("the default scope is this repo: %s", aggyFileText(t))
	}
	if code, out, _ = run("permissions", "add", "allow", "Bash(git log -5)"); code != 0 || !strings.Contains(out, "already") {
		t.Errorf("again: %d %q", code, out)
	}
	// It is in force, with its source.
	code, out, _ = run("permissions", "list")
	if code != 0 || !strings.Contains(out, "Bash(git log -5)  [aggy, this repo]") {
		t.Errorf("list: %d %q", code, out)
	}

	atKeyboard(t, "y\n")
	if code, out, errb = run("permissions", "add", "deny", "Bash(curl *)", "--everywhere"); code != 0 {
		t.Fatalf("add everywhere: %d %q %q", code, out, errb)
	}
	if code, out, _ = run("permissions", "list"); !strings.Contains(out, "Bash(curl *)  [aggy, everywhere]") {
		t.Errorf("list: %d %q", code, out)
	}

	// Removing at the wrong scope names the right one.
	code, _, errb = run("permissions", "remove", "deny", "Bash(curl *)")
	if code != 1 || !strings.Contains(errb, "--everywhere") || !strings.Contains(errb, "[permissions.not_found · run ") {
		t.Errorf("wrong scope: %d %q", code, errb)
	}
	atKeyboard(t, "n\n")
	if code, out, _ = run("permissions", "remove", "deny", "Bash(curl *)", "--everywhere"); code != 1 || !strings.Contains(out, "Nothing changed") {
		t.Errorf("a no: %d %q", code, out)
	}
	atKeyboard(t, "y\n")
	if code, out, errb = run("permissions", "remove", "deny", "Bash(curl *)", "--everywhere"); code != 0 {
		t.Fatalf("remove: %d %q %q", code, out, errb)
	}
	if strings.Contains(aggyFileText(t), "curl") {
		t.Errorf("still there: %s", aggyFileText(t))
	}

	// aggy unwire undoes them.
	if code, out, errb = run("unwire", "--all"); code != 0 {
		t.Fatalf("unwire: %d %q %q", code, out, errb)
	}
	if aggyFileText(t) != "" {
		t.Errorf("after unwire: %s", aggyFileText(t))
	}
}

func TestPermissionsAddRefuses(t *testing.T) {
	isolate(t)
	inRepo(t)
	atKeyboard(t, "y\ny\ny\n")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"permissions", "add", "allow", "Bash(*)"}, "never be passed on"},
		{[]string{"permissions", "add", "allow", "Bash(python3 *)"}, "interpreter"},
		{[]string{"permissions", "add", "allow", "Bash(npm run *)"}, "run command"},
		{[]string{"permissions", "add", "allow", "Bash(python3.12 *)"}, "interpreter"},
		{[]string{"permissions", "add", "deny", "not a rule ("}, "permissions.bad_rule"},
		{[]string{"permissions", "remove", "allow", "Bash(nothing here)"}, "no such allow rule"},
	} {
		code, _, errb := run(tc.args...)
		if code != 1 || !strings.Contains(errb, tc.want) || !strings.Contains(errb, " · run ") {
			t.Errorf("%q: %d %q", tc.args, code, errb)
		}
	}
	if aggyFileText(t) != "" {
		t.Errorf("refusals wrote the file: %s", aggyFileText(t))
	}
}

func TestPermissionsOutsideARepo(t *testing.T) {
	isolate(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	atKeyboard(t, "y\n")
	code, _, errb := run("permissions", "add", "allow", "Bash(ls)")
	if code != 1 || !strings.Contains(errb, "--everywhere") {
		t.Errorf("%d %q", code, errb)
	}
	if code, out, errb := run("permissions", "add", "allow", "Bash(ls)", "--everywhere"); code != 0 {
		t.Errorf("%d %q %q", code, out, errb)
	}
}

// TestRemoveNamesTheFile: a rule from Claude's own file is not edited.
func TestRemoveNamesTheFile(t *testing.T) {
	h := isolate(t)
	inRepo(t)
	settings := filepath.Join(h, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"permissions":{"allow":["Bash(git status)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	atKeyboard(t, "y\n")
	code, _, errb := run("permissions", "remove", "allow", "Bash(git status)")
	if code != 1 || !strings.Contains(errb, "settings.json") || !strings.Contains(errb, "/permissions") {
		t.Errorf("%d %q", code, errb)
	}
	if b, _ := os.ReadFile(settings); !strings.Contains(string(b), "git status") {
		t.Error("Claude's file changed")
	}
}

// TestChangesRefusedFromAModelShell walks the shapes of a model's shell, for
// every command that changes something.
func TestChangesRefusedFromAModelShell(t *testing.T) {
	isolate(t)
	inRepo(t)
	commands := [][]string{
		{"permissions", "add", "allow", "Bash(gh pr list)"},
		{"permissions", "add", "deny", "Bash(x y)", "--everywhere"},
		{"permissions", "suggest"},
		{"permissions", "suggest", "--all"},
		{"config", "set", "permission-mode", "auto"},
		{"config", "set", "harness.grok", "off"},
		{"trust", "."},
		{"trust", "--opencode", "."},
	}
	shapes := []struct {
		name string
		env  map[string]string
		tty  bool
	}{
		{"no terminal", nil, false},
		{"borrow child", map[string]string{"AGGY_DELEGATED": "1"}, true},
		{"on-screen session's tool shell", map[string]string{"AGGY_SOCKET": "/tmp/s", "AGGY_PARENT": "p", "AGGY_VENDOR": "claude"}, true},
		{"on-screen session's tool shell, no terminal", map[string]string{"AGGY_SOCKET": "/tmp/s", "AGGY_PARENT": "p", "AGGY_VENDOR": "claude"}, false},
		{"--direct session's tool shell", map[string]string{"AGGY_VENDOR": "claude"}, true},
	}
	cfgPath, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	for _, shape := range shapes {
		for _, args := range commands {
			t.Run(shape.name+"/"+strings.Join(args, " "), func(t *testing.T) {
				atKeyboard(t, "y\ny\ny\n")
				stdinIsTerminal = func() bool { return shape.tty }
				for k, v := range shape.env {
					t.Setenv(k, v)
				}
				code, out, errb := run(args...)
				if code != 1 {
					t.Errorf("exit %d: %q %q", code, out, errb)
				}
				if !strings.Contains(errb, " · run ") {
					t.Errorf("not tagged: %q", errb)
				}
				if aggyFileText(t) != "" {
					t.Errorf("wrote the rules: %s", aggyFileText(t))
				}
				if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
					t.Errorf("wrote the config: %v", err)
				}
				if p, _ := trustFile(); fileExists(p) {
					t.Error("wrote the trust file")
				}
			})
		}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func TestConfigSet(t *testing.T) {
	isolate(t)
	inRepo(t)
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const before = "# mine, by hand\n[switch]\nkey = \"ctrl+g\"\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, _ := run("config", "set", "default-harness", "grok", "--dry-run")
	if code != 0 || !strings.Contains(out, "default_vendor") {
		t.Errorf("dry run: %d %q", code, out)
	}
	if b, _ := os.ReadFile(path); string(b) != before {
		t.Error("the dry run wrote")
	}
	code, _, errb := run("config", "set", "default-harness", "grok")
	if code != 1 || !strings.Contains(errb, "[permissions.not_at_keyboard · run ") {
		t.Errorf("no terminal: %d %q", code, errb)
	}

	for _, tc := range []struct{ key, val string }{
		{"default-harness", "grok"},
		{"permission-mode", "auto"},
		{"model.claude", "opus"},
		{"harness.codex", "off"},
	} {
		atKeyboard(t, "y\n")
		if code, out, errb := run("config", "set", tc.key, tc.val); code != 0 {
			t.Fatalf("%s: %d %q %q", tc.key, code, out, errb)
		}
	}
	cfg, err := config.LoadDefault()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultVendor != "grok" || cfg.Permissions.Mode != "auto" || cfg.Vendors.Claude.Model != "opus" || !cfg.Vendors.Codex.Off {
		t.Errorf("%+v", cfg)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "# mine, by hand") || !strings.Contains(string(b), `key = "ctrl+g"`) {
		t.Errorf("the person's text is gone: %s", b)
	}
	code, out, _ = run("config", "list")
	for _, w := range []string{"default-harness    grok", "permission-mode    auto", "harness.codex", "off"} {
		if code != 0 || !strings.Contains(out, w) {
			t.Errorf("list lacks %q: %s", w, out)
		}
	}
	if code, out, _ = run("config", "set", "model.claude", "opus"); code != 0 || !strings.Contains(out, "already") {
		t.Errorf("again: %d %q", code, out)
	}
	for _, tc := range []struct{ key, val string }{
		{"permission-mode", "bypassPermissions"},
		{"default-harness", "nope"},
		{"harness.codex", "maybe"},
		{"model.nope", "x"},
		{"nope", "x"},
	} {
		code, _, errb := run("config", "set", tc.key, tc.val)
		if code != 1 || !strings.Contains(errb, "[config.bad_value · run ") {
			t.Errorf("%s=%s: %d %q", tc.key, tc.val, code, errb)
		}
	}
	for _, tc := range []struct{ key, val string }{{"harness.codex", "on"}, {"permission-mode", "none"}, {"model.claude", "none"}, {"default-harness", "none"}} {
		if code, out, errb := run("config", "set", tc.key, tc.val); code != 0 {
			t.Fatalf("%s: %d %q %q", tc.key, code, out, errb)
		}
	}
	if b, _ := os.ReadFile(path); strings.Contains(string(b), "off") || strings.Contains(string(b), "opus") || strings.Contains(string(b), "default_vendor") {
		t.Errorf("not removed: %s", b)
	}
	if code, out, errb := run("unwire", "--all"); code != 0 {
		t.Fatalf("unwire: %d %q %q", code, out, errb)
	}
	if b, _ := os.ReadFile(path); string(b) != before {
		t.Errorf("after unwire: %q", b)
	}
}

func TestHarnessOffRefused(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	cfg, err := config.Parse("[vendors.grok]\noff = true\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := allowedIn(cfg, repo, "grok"); err == nil || !strings.Contains(err.Error(), "turned off") {
		t.Errorf("%v", err)
	}
}

func TestDoctorReportsFileMistakesByLine(t *testing.T) {
	isolate(t)
	writePermissions(t, "{\n  \"permissions\": {\n    \"allow\": [\n      \"Bash(*)\"\n    ]\n  }\n}\n")
	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("# a\n\nnope = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var codes []string
	for _, f := range permissionFindings(t.Context()) {
		codes = append(codes, f.Code+": "+f.Message)
		if strings.Contains(f.Message, "Bash") || strings.Contains(f.Message, "nope") {
			t.Errorf("a report must not carry what the person wrote: %q", f.Message)
		}
	}
	joined := strings.Join(codes, "\n")
	for _, w := range []string{"permissions-file: line 4:", "config-file: line 3:"} {
		if !strings.Contains(joined, w) {
			t.Errorf("no %q in:\n%s", w, joined)
		}
	}
}

// autoSession writes a session file for repo, in Claude's project folder.
func autoSession(t *testing.T, repo, name string, commands map[string]int) string {
	t.Helper()
	home, err := transcript.Home("claude", os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "projects", transcript.ClaudeProjectDir(repo))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	line := func(v any) {
		j, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(j)
		b.WriteByte('\n')
	}
	line(map[string]any{"type": "user", "cwd": repo, "permissionMode": "auto"})
	id := 0
	for cmd, n := range commands {
		for range n {
			id++
			use := fmt.Sprintf("t%d", id)
			line(map[string]any{"cwd": repo, "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_use", "id": use, "name": "Bash", "input": map[string]any{"command": cmd}}}}})
			line(map[string]any{"cwd": repo, "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": use}}}})
		}
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(b.String()); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSuggestFlow(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	autoSession(t, repo, "a.jsonl", map[string]int{"gh pr list": 3, "git log -3": 2})

	// No terminal, nothing is shown or added.
	code, out, errb := run("permissions", "suggest")
	if code != 1 || !strings.Contains(errb, "not_at_keyboard") || strings.Contains(out, "gh pr list") {
		t.Errorf("no terminal: %d %q %q", code, out, errb)
	}

	atKeyboard(t, "n\n")
	code, out, errb = run("permissions", "suggest")
	if code != 0 || !strings.Contains(out, "gh pr list") || !strings.Contains(out, "ran 3 times") || strings.Contains(out, "git log -3") {
		t.Fatalf("first: %d %q %q", code, out, errb)
	}
	if aggyFileText(t) != "" {
		t.Error("a no added a rule")
	}
	// Turned down, it is not asked again, and a new one is.
	autoSession(t, repo, "a.jsonl", map[string]int{"gh pr list": 1, "gh pr view": 3})
	atKeyboard(t, "y\n")
	code, out, errb = run("permissions", "suggest")
	if code != 0 || strings.Contains(out, "gh pr list") || !strings.Contains(out, "gh pr view") || !strings.Contains(out, "Added") {
		t.Fatalf("second: %d %q %q", code, out, errb)
	}
	if !strings.Contains(aggyFileText(t), "Bash(gh pr view)") {
		t.Errorf("not added: %s", aggyFileText(t))
	}
	atKeyboard(t, "")
	if code, out, _ = run("permissions", "suggest"); code != 0 || !strings.Contains(out, "Nothing new") {
		t.Errorf("third: %d %q", code, out)
	}
}

func TestSuggestAll(t *testing.T) {
	isolate(t)
	here := inRepo(t)
	other := tempRepo(t)
	autoSession(t, here, "a.jsonl", map[string]int{"gh pr list": 3})
	autoSession(t, other, "b.jsonl", map[string]int{"gh run list": 3})

	atKeyboard(t, "n\n")
	if code, out, errb := run("permissions", "suggest"); code != 0 || strings.Contains(out, "gh run list") {
		t.Errorf("this repo only: %d %q %q", code, out, errb)
	}
	atKeyboard(t, "y\n")
	code, out, errb := run("permissions", "suggest", "--all")
	if code != 0 || !strings.Contains(out, "gh run list") {
		t.Fatalf("all: %d %q %q", code, out, errb)
	}
	key, err := safefs.CanonicalPath(other)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(mustPermissionsFile(t))
	f, err := permissions.ParseAggyFile(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Repos[key].Allow) != 1 || len(f.Permissions.Allow) != 0 {
		t.Errorf("a rule stays with its own repo: %s", b)
	}
}

func mustPermissionsFile(t *testing.T) string {
	t.Helper()
	p, err := permissionsFile()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestSuggestAtLaunch: the job never makes an allow, resumes where it stopped,
// and says one line only when there is something new.
func TestSuggestAtLaunch(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	key, err := safefs.CanonicalPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	path := autoSession(t, repo, "a.jsonl", map[string]int{"gh pr list": 3})
	ctx := t.Context()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	stop, note := suggestAtLaunch(ctx, s, repo)
	if note != "" {
		t.Errorf("nothing scanned yet, but: %q", note)
	}
	waitForCandidates(t, s, key, 1)
	stop()
	if aggyFileText(t) != "" {
		t.Errorf("a candidate became a rule: %s", aggyFileText(t))
	}
	row, err := s.SuggestFileState(ctx, path)
	if err != nil || row.Offset == 0 {
		t.Fatalf("%+v, %v", row, err)
	}

	stop, note = suggestAtLaunch(ctx, s, repo)
	stop()
	if !strings.Contains(note, "1 command") || !strings.Contains(note, "aggy permissions suggest") {
		t.Errorf("note: %q", note)
	}
	stop, note = suggestAtLaunch(ctx, s, repo)
	stop()
	if note != "" {
		t.Errorf("said twice: %q", note)
	}
	// No line is counted twice.
	c, err := s.SuggestCandidates(ctx, key, 1)
	if err != nil || len(c) != 1 || c[0].Count != 3 {
		t.Errorf("%+v, %v", c, err)
	}
}

func waitForCandidates(t *testing.T, s *store.Store, key string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := s.SuggestCandidates(t.Context(), key, 1); err == nil && len(c) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no candidates in time")
}

// TestSuggestOtherFolderInSessionDir: lines from a folder that only shares the
// session folder's name are not this repo's.
func TestSuggestOtherFolderInSessionDir(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	key, err := safefs.CanonicalPath(repo)
	if err != nil {
		t.Fatal(err)
	}
	home, err := transcript.Home("claude", os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, "projects", transcript.ClaudeProjectDir(repo))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	text := `{"cwd":"/elsewhere","permissionMode":"auto"}` + "\n"
	for i := range 3 {
		text += fmt.Sprintf(`{"cwd":"/elsewhere","message":{"content":[{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"gh pr list"}}]}}`+"\n", i)
		text += fmt.Sprintf(`{"cwd":"/elsewhere","message":{"content":[{"type":"tool_result","tool_use_id":"t%d"}]}}`+"\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.jsonl"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := store.OpenDefault(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	scanRepo(t.Context(), s, repo)
	if c, err := s.SuggestCandidates(t.Context(), key, 1); err != nil || len(c) != 0 {
		t.Errorf("%+v, %v", c, err)
	}
}
