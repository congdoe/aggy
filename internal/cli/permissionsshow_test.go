package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/store"
)

func TestPermissionsShow(t *testing.T) {
	h := isolate(t)
	repo := inRepo(t)
	writePermissions(t, `{"permissions":{"defaultMode":"plan","allow":["Bash(git log -5)","Bash(*)","Bash(git status *)","Bash(gh pr list)"],"ask":["Bash(git push *)"],"deny":["Bash(curl *)","Read(./.env)"]}}`)
	settings := filepath.Join(h, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settings, []byte(`{"permissions":{"allow":["Bash(git status)","Bash(*)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Not read: this repo's copy is not git's to answer for in a bare folder.
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.local.json"), []byte(`{"permissions":{"allow":["Bash(curl evil)"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := store.DefaultPath()
	if err != nil {
		t.Fatal(err)
	}

	code, out, errb := run("permissions", "show")
	if code != 0 {
		t.Fatalf("%d %q", code, errb)
	}
	for _, w := range []string{
		"not trusted",
		"Mode: plan (aggy, everywhere)",
		"Safety policy: on",
		"Bash(git log -5)  [aggy, everywhere]",
		"allow Bash(gh pr list)  [aggy, everywhere]",
		"waits for trust",
		"Bash(git status)  [Claude, your settings] Claude applies this itself",
		"aggy does not pass this on, but Claude applies this itself",
		"Claude Code: reads its own settings files, and takes 2 allow, 1 ask, and 2 deny from aggy",
		"Grok Build: reads Claude's settings files itself",
		"Codex: takes none of these yet",
		"allow Bash(*)  [aggy, everywhere]",
		"blanket shell access",
		"allow Bash(git status *)  [aggy, everywhere] on OpenCode",
		"a rule here matches exactly",
		"ask Bash(git push *)  [aggy, everywhere] on Grok",
		"Grok has no flag",
		"allow Bash(curl evil)  [Claude, this repo's local settings]",
		"opencode.db",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("no %q in:\n%s", w, out)
		}
	}
	// It only reads, and works from a model's shell, as it changes nothing.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("show opened the index: %v", err)
	}
	if code, _, _ := run("permissions", "show", "extra"); code != 2 {
		t.Errorf("extra argument: %d", code)
	}
}
