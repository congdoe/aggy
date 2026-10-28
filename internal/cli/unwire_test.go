package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/manifest"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
)

// isolate points HOME and AGGY_HOME at temp dirs and returns HOME.
func isolate(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv(home.Env, t.TempDir())
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("GROK_HOME", "")
	return h
}

func run(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// applyAll applies patches through the manifest, as setup would.
func applyAll(t *testing.T, runID string, ps ...patch.Patch) []store.Change {
	t.Helper()
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	m, err := manifest.Open(s)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := m.Run(ctx, manifest.Write, runID, ps, nil)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func appendOnce(text string) func([]byte) ([]byte, error) {
	return func(b []byte) ([]byte, error) {
		if bytes.Contains(b, []byte(text)) {
			return b, nil
		}
		return append(append([]byte{}, b...), text...), nil
	}
}

func TestUnwireArgs(t *testing.T) {
	isolate(t)
	for _, args := range [][]string{
		{"unwire"},
		{"unwire", "--all", "--run", "r"},
		{"unwire", "--all", "--force"},
		{"unwire", "--run", "r", "--force", "--yes"},
		{"unwire", "--change", "c", "--yes"},
		{"unwire", "--all", "extra"},
	} {
		if code, _, _ := run(args...); code != 2 {
			t.Errorf("%v exit = %d, want 2", args, code)
		}
	}
	// No index: nothing to undo, and none is created.
	code, out, _ := run("unwire", "--all")
	if code != 0 || !strings.Contains(out, "Nothing to undo") {
		t.Errorf("no index: exit %d, %q", code, out)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv(home.Env), store.FileName)); err == nil {
		t.Error("unwire created the index")
	}
}

func TestUnwireCommand(t *testing.T) {
	h := isolate(t)
	file := filepath.Join(h, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := "model = \"o\"\n"
	if err := os.WriteFile(file, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	cs := applyAll(t, "r", patch.Patch{File: file, ExpectedHash: patch.Hash([]byte(orig)), Transform: appendOnce("x = 1\n")})
	id := cs[0].ChangeID

	code, out, errOut := run("unwire", "--all", "--dry-run")
	if code != 0 || !strings.Contains(out, "-x = 1") || !strings.Contains(out, "would be restored") {
		t.Errorf("dry run: exit %d\n%s\n%s", code, out, errOut)
	}
	if b, _ := os.ReadFile(file); string(b) == orig {
		t.Error("dry run restored the file")
	}

	// Drift: refused, reported, exit 1, file untouched.
	edited := orig + "x = 1\nmine = 2\n"
	if err := os.WriteFile(file, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = run("unwire", "--all")
	if code != 1 || !strings.Contains(errOut, "refused") || !strings.Contains(errOut, id) {
		t.Errorf("drift: exit %d, %q", code, errOut)
	}
	// --force without --yes shows what it would overwrite, changes nothing.
	code, out, errOut = run("unwire", "--change", id, "--force")
	if code != 1 || !strings.Contains(out, "-mine = 2") || !strings.Contains(errOut, "--force --yes") {
		t.Errorf("unconfirmed force: exit %d\n%s\n%s", code, out, errOut)
	}
	if b, _ := os.ReadFile(file); string(b) != edited {
		t.Errorf("unconfirmed force changed the file: %q", b)
	}
	code, out, errOut = run("unwire", "--change", id, "--force", "--yes")
	if code != 0 || !strings.Contains(out, "restored") || !strings.Contains(out, "saved in") {
		t.Errorf("force: exit %d\n%s\n%s", code, out, errOut)
	}
	if b, _ := os.ReadFile(file); string(b) != orig {
		t.Errorf("forced file = %q", b)
	}
	code, out, _ = run("unwire", "--all")
	if code != 0 || !strings.Contains(out, "Nothing to undo") {
		t.Errorf("after undo: exit %d, %q", code, out)
	}
}

// TestUnwireChangeRefusesWhileNewer (PR #6 finding 5): --change on an
// older change while aggy's newer change to the same file is in place is
// refused, forced or not, and names the newer change. On a8b4c37 the
// newer change was taken for drift, and --force wiped it.
func TestUnwireChangeRefusesWhileNewer(t *testing.T) {
	h := isolate(t)
	file := filepath.Join(h, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	orig := "model = \"o\"\n"
	if err := os.WriteFile(file, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	a := applyAll(t, "A", patch.Patch{File: file, ExpectedHash: patch.Hash([]byte(orig)), Transform: appendOnce("a = 1\n")})
	mid := orig + "a = 1\n"
	b := applyAll(t, "B", patch.Patch{File: file, ExpectedHash: patch.Hash([]byte(mid)), Transform: appendOnce("b = 2\n")})
	for _, args := range [][]string{
		{"unwire", "--change", a[0].ChangeID},
		{"unwire", "--change", a[0].ChangeID, "--force", "--yes"},
	} {
		code, _, errOut := run(args...)
		if code != 1 || !strings.Contains(errOut, b[0].ChangeID) {
			t.Errorf("%v: exit %d, %q, want a refusal naming %s", args, code, errOut, b[0].ChangeID)
		}
		if got, _ := os.ReadFile(file); string(got) != mid+"b = 2\n" {
			t.Fatalf("%v changed the file: %q", args, got)
		}
	}
	if code, _, errOut := run("unwire", "--all"); code != 0 {
		t.Fatalf("--all: exit %d: %s", code, errOut)
	}
	if got, _ := os.ReadFile(file); string(got) != orig {
		t.Errorf("after --all = %q", got)
	}
}
