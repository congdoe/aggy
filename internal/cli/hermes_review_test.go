package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/safefs"
)

// setTrustNever sets the never-ask answer on dir's folder in the trust list.
func setTrustNever(t *testing.T, dir string) {
	t.Helper()
	tg, err := trustTargetFor(dir)
	if err != nil {
		t.Fatal(err)
	}
	path, err := trustFile()
	if err != nil {
		t.Fatal(err)
	}
	f, err := readTrustRecord(path)
	if err != nil {
		t.Fatal(err)
	}
	a := f.Folders[tg.key]
	a.Never = true
	f.Folders[tg.key] = a
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestHermesRootSuffix (#134 review): a HERMES_DATA_DIR_SUFFIX that is not
// a plain name is refused, and safefs agrees on what a plain name is.
func TestHermesRootSuffix(t *testing.T) {
	for _, c := range []struct {
		suffix string
		ok     bool
	}{{"", true}, {"-work", true}, {"/x", false}, {"..", false}, {".", false}, {"a/../../b", false}, {`a\b`, false}, {"a\nb", false}} {
		env := map[string]string{"HOME": "/h", "HERMES_DATA_DIR_SUFFIX": c.suffix}
		got, err := hermesRoot(func(k string) string { return env[k] })
		if (err == nil) != c.ok || (c.ok && got != "/h/.hermes"+c.suffix) {
			t.Errorf("%q: %q, %v", c.suffix, got, err)
		}
		if safefs.HermesSuffixOK(c.suffix) != c.ok {
			t.Errorf("safefs disagrees on %q", c.suffix)
		}
	}
}

// TestHermesProfileUnwire (#134 review): a profile aggy made is recorded, and
// aggy unwire deletes it (after taking its block out) with hermes profile
// delete; a dry run only lists it; a profile aggy did not make is never
// recorded or deleted.
func TestHermesProfileUnwire(t *testing.T) {
	repo, dir, bin, s := hermesWorld(t)
	tickTrustRows(t, repo, trustRowHermes)
	// aggy unwire finds hermes on PATH.
	pathDir := t.TempDir()
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pathDir, "hermes"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", pathDir+":/usr/bin:/bin")
	in := hermesInput{s: s, caps: hermesCaps, path: bin, cwd: repo, dir: dir, runDir: filepath.Join(os.Getenv("AGGY_HOME"), "run"),
		plan: parity.PolicyPlan{OnScreen: true}, version: "0.23.1"}
	ctx := context.Background()
	out, err := prepareHermes(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(out.profileDir); err != nil {
		t.Fatal(err)
	}
	unwire := func(args ...string) (string, string, int) {
		var o, e strings.Builder
		code := runUnwire(args, &o, &e)
		return o.String(), e.String(), code
	}
	o, e, code := unwire("--all", "--dry-run")
	if code != 0 || !strings.Contains(o, "would be deleted: aggy's Hermes profile") {
		t.Fatalf("dry run: %d %q %q", code, o, e)
	}
	if _, err := os.Stat(out.profileDir); err != nil {
		t.Fatalf("a dry run deleted it: %v", err)
	}
	o, e, code = unwire("--all")
	if code != 0 || !strings.Contains(o, "deleted aggy's Hermes profile") {
		t.Fatalf("unwire: %d %q %q", code, o, e)
	}
	if _, err := os.Stat(out.profileDir); !os.IsNotExist(err) {
		t.Fatalf("still there: %v", err)
	}
	if o, _, _ := unwire("--all"); strings.Contains(o, "Hermes profile") {
		t.Fatalf("deleted twice: %q", o)
	}
	// A profile already there (the person's, or one aggy no longer records)
	// gets no marker.
	if err := os.MkdirAll(out.profileDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareHermes(ctx, in); err != nil {
		t.Fatal(err)
	}
	if o, _, _ := unwire("--all", "--dry-run"); strings.Contains(o, "Hermes profile") {
		t.Fatalf("a profile aggy did not make: %q", o)
	}
}

// TestHermesReadBackEveryLaunch (#134 review): with aggy's block already in
// place and unchanged, a launch still asks Hermes how it reads approvals.mode
// and the hooks, and refuses on a mismatch (a broken line before the block,
// say, that Hermes reads differently).
func TestHermesReadBackEveryLaunch(t *testing.T) {
	repo, dir, bin, s := hermesWorld(t)
	tickTrustRows(t, repo, trustRowHermes)
	in := hermesInput{s: s, caps: hermesCaps, path: bin, cwd: repo, dir: dir, runDir: filepath.Join(os.Getenv("AGGY_HOME"), "run"),
		plan: parity.PolicyPlan{OnScreen: true}, version: "0.23.1"}
	ctx := context.Background()
	if _, err := prepareHermes(ctx, in); err != nil {
		t.Fatal(err)
	}
	real := hermesRun
	var gets int
	hermesRun = func(ctx context.Context, cmd adapter.Command) (adapter.Output, error) {
		if slices.Contains(cmd.Args, "get") && slices.Contains(cmd.Args, "approvals.mode") {
			gets++
			return adapter.Output{Stdout: []byte(`"smart"` + "\n")}, nil
		}
		return real(ctx, cmd)
	}
	t.Cleanup(func() { hermesRun = real })
	_, err := prepareHermes(ctx, in)
	if gets == 0 || hermesCode(err) != diag.CodeHermesProfile || !strings.Contains(err.Error(), "approvals.mode") {
		t.Fatalf("gets %d, err %v", gets, err)
	}
}
