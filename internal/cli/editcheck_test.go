package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/store"
)

// gateFixture is an edit fixture whose main checkout lists one check,
// sh t.sh, and ignores .aggy as aggy's parity does. The child writes t.sh,
// which records its working dir in a marker and then runs script (REPO
// in it is the main checkout), and its own .aggy/config.toml naming
// evil.sh, which must never run.
func gateFixture(t *testing.T, script string) (f *borrowFixture, id, wt, ran, evil string) {
	t.Helper()
	f, markers := editFixture(t)
	t.Cleanup(func() { noMarkers(t, markers) })
	checkTools(t, "sh")
	gitIn(t, f.repo, "checkout", "--", "a.go")
	mustWrite(t, filepath.Join(f.repo, ".git", "info", "exclude"), ".aggy/\n", 0o600)
	mustWrite(t, filepath.Join(f.repo, ".aggy", "config.toml"), `check = [["sh", "t.sh"]]`+"\n", 0o600)
	dir := t.TempDir()
	ran, evil = filepath.Join(dir, "ran"), filepath.Join(dir, "evil")
	id, wt = editBorrow(t, f, map[string]string{
		"a.go":              "package a\n\nfunc Edited() {}\n",
		"t.sh":              "pwd > " + ran + "\n" + strings.ReplaceAll(script, "REPO", f.repo) + "\n",
		"evil.sh":           "touch " + evil + "\n",
		".aggy/config.toml": `check = [["sh", "evil.sh"]]` + "\n",
	})
	return f, id, wt, ran, evil
}

func checkEdit(f *borrowFixture, in string, tty bool, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := checkEditWith(context.Background(), args, f.env, defaultRepoCheckEnv(), strings.NewReader(in), tty, &out, &errb)
	return code, out.String(), errb.String()
}

func gateResult(t *testing.T, id string) string {
	t.Helper()
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	wt, err := s.BorrowWorktree(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return wt.Check
}

// TestEditCheckGate (T6691, W-5): aggy check <id> shows the diff stat,
// takes the check list from the main checkout, never the worktree's
// config, says the worktree holds the child's code, runs the checks in
// the worktree only on y or --yes, and records the result.
func TestEditCheckGate(t *testing.T) {
	f, id, wt, ran, evil := gateFixture(t, "exit 0")

	code, out, errOut := checkEdit(f, "", false, id)
	if code != 1 || !strings.Contains(out, "a.go") || !strings.Contains(out, "code the child wrote") || !strings.Contains(out, "Checks not run") {
		t.Errorf("no tty: code %d: %s %s", code, out, errOut)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the checks ran without a yes")
	}
	if r := gateResult(t, id); r != "" {
		t.Errorf("recorded %q without a run", r)
	}
	if code, out, _ = checkEdit(f, "n\n", true, id); code != 1 || !strings.Contains(out, "Checks skipped") {
		t.Errorf("answered n: code %d: %s", code, out)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the checks ran on n")
	}
	if r := gateResult(t, id); r != store.CheckSkipped {
		t.Errorf("recorded %q, want skipped", r)
	}

	code, out, errOut = checkEdit(f, "", false, id, "--yes")
	if code != 0 {
		t.Fatalf("--yes: code %d: %s %s", code, out, errOut)
	}
	if i, j := strings.Index(out, "a.go"), strings.Index(out, "ok   sh t.sh"); i < 0 || j < i {
		t.Errorf("want the stat before the checks:\n%s", out)
	}
	if data, err := os.ReadFile(ran); err != nil || resolvePath(strings.TrimSpace(string(data))) != resolvePath(wt) {
		t.Errorf("the check ran in %q, want %s (%v)", data, wt, err)
	}
	if r := gateResult(t, id); r != store.CheckPass {
		t.Errorf("recorded %q, want pass", r)
	}
	if code, out, _ := accept(f, "", false, id); code != 1 || !strings.Contains(out, "checks: passed") {
		t.Errorf("accept: code %d: %s", code, out)
	}
	if _, err := os.Stat(evil); err == nil {
		t.Error("the worktree's own config ran")
	}
}

// TestAcceptGate (T6691): a failed check does not block accept but warns;
// accept --check runs the checks; on a terminal accept offers them first.
func TestAcceptGate(t *testing.T) {
	t.Run("failed check warns", func(t *testing.T) {
		f, id, _, ran, evil := gateFixture(t, "exit 1")
		if code, out, _ := checkEdit(f, "", false, id, "--yes"); code != 1 || !strings.Contains(out, "FAIL sh t.sh") {
			t.Fatalf("code %d: %s", code, out)
		}
		if r := gateResult(t, id); r != store.CheckFail {
			t.Errorf("recorded %q, want fail", r)
		}
		_ = os.Remove(ran)
		code, out, errOut := accept(f, "", false, id, "--yes")
		if code != 0 || !strings.Contains(out, "checks: FAILED") || !strings.Contains(out, "Warning: the repo's checks FAILED") {
			t.Errorf("code %d: %s %s", code, out, errOut)
		}
		if _, err := os.Stat(ran); err == nil {
			t.Error("accept --yes ran the checks again")
		}
		if _, err := os.Stat(evil); err == nil {
			t.Error("the worktree's own config ran")
		}
	})
	t.Run("accept --check", func(t *testing.T) {
		f, id, _, ran, _ := gateFixture(t, "exit 0")
		code, out, errOut := accept(f, "", false, id, "--check", "--yes")
		if code != 0 || !strings.Contains(out, "code the child wrote") || !strings.Contains(out, "ok   sh t.sh") {
			t.Errorf("code %d: %s %s", code, out, errOut)
		}
		if _, err := os.Stat(ran); err != nil {
			t.Error("--check did not run the checks")
		}
	})
	t.Run("accept asks on a terminal", func(t *testing.T) {
		f, id, _, ran, _ := gateFixture(t, "exit 0")
		code, out, errOut := accept(f, "n\ny\n", true, id)
		if code != 0 || !strings.Contains(out, "Run them?") || !strings.Contains(out, "Checks skipped") || !strings.Contains(out, "Applied") {
			t.Errorf("code %d: %s %s", code, out, errOut)
		}
		if _, err := os.Stat(ran); err == nil {
			t.Error("the checks ran on n")
		}
		if data, _ := os.ReadFile(filepath.Join(f.repo, "a.go")); !strings.Contains(string(data), "Edited") {
			t.Errorf("a.go = %q", data)
		}
	})
	t.Run("accept --yes without checks records skipped", func(t *testing.T) {
		f, id, _, ran, _ := gateFixture(t, "exit 0")
		if code, out, errOut := accept(f, "", false, id, "--yes"); code != 0 || !strings.Contains(out, "checks: not run") {
			t.Errorf("code %d: %s %s", code, out, errOut)
		}
		if _, err := os.Stat(ran); err == nil {
			t.Error("accept --yes ran the checks unasked")
		}
	})
}

// TestCheckRefusesEditWorktree (T6691, W-5): aggy check run by hand in an
// edit worktree refuses, so the child's own config never runs.
func TestCheckRefusesEditWorktree(t *testing.T) {
	f, _, wt, _, evil := gateFixture(t, "exit 0")
	for _, dir := range []string{wt, filepath.Join(wt, ".aggy")} {
		e := defaultRepoCheckEnv()
		e.getwd = func() (string, error) { return dir, nil }
		var out, errb bytes.Buffer
		if code := e.check(context.Background(), nil, &out, &errb); code != 2 || !strings.Contains(errb.String(), "edit worktree") {
			t.Errorf("%s: code %d: %s %s", dir, code, out.String(), errb.String())
		}
	}
	if _, err := os.Stat(evil); err == nil {
		t.Error("the worktree's own config ran")
	}
	// The main checkout itself is not refused.
	e := defaultRepoCheckEnv()
	e.getwd = func() (string, error) { return f.repo, nil }
	var out, errb bytes.Buffer
	if code := e.check(context.Background(), nil, &out, &errb); strings.Contains(errb.String(), "edit worktree") {
		t.Errorf("main checkout: code %d: %s", code, errb.String())
	}
}

// TestGateChecksThatWrite (K46-1): a check that changes the worktree, or
// the main checkout, makes accept refuse; the old patch is never applied
// and no pass is recorded.
func TestGateChecksThatWrite(t *testing.T) {
	t.Run("worktree", func(t *testing.T) {
		f, id, _, _, _ := gateFixture(t, "echo '// gofmt' >> a.go\nexit 0")
		code, out, errOut := accept(f, "y\ny\n", true, id)
		if code != 1 || !strings.Contains(errOut, "the checks changed files in the worktree") || strings.Contains(out, "Applied") {
			t.Errorf("code %d: %s %s", code, out, errOut)
		}
		if data, _ := os.ReadFile(filepath.Join(f.repo, "a.go")); strings.Contains(string(data), "Edited") {
			t.Errorf("applied: a.go = %q", data)
		}
		if r := gateResult(t, id); r != store.CheckFail {
			t.Errorf("recorded %q, want fail", r)
		}
		if code, out, errOut := checkEdit(f, "", false, id, "--yes"); code != 1 || !strings.Contains(errOut, "changed after the borrow ended") {
			t.Errorf("check again: code %d: %s %s", code, out, errOut)
		}
	})
	t.Run("main checkout", func(t *testing.T) {
		f, id, _, _, _ := gateFixture(t, "echo '// dirty' >> REPO/a.go\nexit 0")
		code, out, errOut := accept(f, "", false, id, "--check", "--yes")
		if code != 1 || !strings.Contains(errOut, "has changes in paths the diff touches") || strings.Contains(out, "Applied") {
			t.Errorf("code %d: %s %s", code, out, errOut)
		}
		if data, _ := os.ReadFile(filepath.Join(f.repo, "a.go")); strings.Contains(string(data), "Edited") {
			t.Errorf("applied: a.go = %q", data)
		}
	})
}

// TestGateRefusesTmpRepo (K46-2): a main checkout in a temp dir the
// child's sandbox may write gets no check run: its check list may be the
// child's.
func TestGateRefusesTmpRepo(t *testing.T) {
	f, id, _, ran, _ := gateFixture(t, "exit 0")
	f.env.tmpDirs = func() []string { return []string{"", filepath.Dir(f.repo)} }
	code, out, errOut := checkEdit(f, "", false, id, "--yes")
	if code != 1 || !strings.Contains(errOut, "refusing to run the checks") || !strings.Contains(errOut, "temp dirs") {
		t.Errorf("code %d: %s %s", code, out, errOut)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Error("the checks ran")
	}
}

// TestAcceptRefusesTmpState (T6750, K46-4): accept writes its patch under
// aggy's state dir, so a state dir in a temp dir the child's sandbox may
// write is refused before anything is shown or applied. On darwin the
// comparison ignores case.
func TestAcceptRefusesTmpState(t *testing.T) {
	f, _ := editFixture(t)
	gitIn(t, f.repo, "checkout", "--", "a.go")
	id, _ := editBorrow(t, f, map[string]string{"a.go": "package a\n// edited\n"})
	tmp := filepath.Dir(f.state)
	if runtime.GOOS == "darwin" {
		tmp = strings.ToUpper(tmp)
	}
	f.env.tmpDirs = func() []string { return []string{"", tmp} }
	code, out, errOut := accept(f, "", false, id, "--yes")
	if code != 1 || !strings.Contains(errOut, "aggy's state dir") || !strings.Contains(errOut, "set AGGY_HOME") || !strings.Contains(errOut, "accept.state_temp_dir") || strings.Contains(out, "Applied") {
		t.Errorf("code %d: %s %s", code, out, errOut)
	}
	if data, _ := os.ReadFile(filepath.Join(f.repo, "a.go")); strings.Contains(string(data), "edited") {
		t.Errorf("applied: a.go = %q", data)
	}
}
