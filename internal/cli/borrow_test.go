package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

func TestParseBorrowArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    borrowArgs
		wantErr string
	}{
		{"flags after the task", []string{"research", "what changed?", "--path", "a.go", "--constraint=cite", "--vendor", "grok"},
			borrowArgs{kind: "research", task: "what changed?", paths: []string{"a.go"}, constraints: []string{"cite"}, vendor: "grok"}, ""},
		{"repeated flags first", []string{"--path", "a", "--path=b", "review", "look"},
			borrowArgs{kind: "review", task: "look", paths: []string{"a", "b"}}, ""},
		{"dash task after --", []string{"news", "--", "--weird task"}, borrowArgs{kind: "news", task: "--weird task"}, ""},
		{"help", []string{"-h"}, borrowArgs{help: true}, ""},
		{"fresh", []string{"news", "x", "--fresh"}, borrowArgs{kind: "news", task: "x", fresh: true}, ""},
		{"unknown flag", []string{"news", "x", "--model", "m"}, borrowArgs{}, "unknown flag --model"},
		{"missing value", []string{"news", "x", "--path"}, borrowArgs{}, "needs a value"},
		{"no task", []string{"news"}, borrowArgs{}, "want a kind and one quoted task"},
		{"unquoted task", []string{"news", "what", "changed"}, borrowArgs{}, "want a kind and one quoted task"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseBorrowArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.kind != tt.want.kind || got.task != tt.want.task || got.vendor != tt.want.vendor || got.help != tt.want.help || got.fresh != tt.want.fresh ||
				!slices.Equal(got.paths, tt.want.paths) || !slices.Equal(got.constraints, tt.want.constraints) {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// borrowFixture is a temp HOME and AGGY_HOME with the fakes installed
// under their vendor names in a bin dir.
type borrowFixture struct {
	fakes    fakecli.Fakes
	bin      string
	record   string
	scenario map[string]string
	env      borrowEnv
	repo     string
	state    string
}

func newBorrowFixture(t *testing.T, vendors ...adapter.Vendor) *borrowFixture {
	t.Helper()
	f := &borrowFixture{fakes: fakecli.Build(t), bin: t.TempDir(), record: filepath.Join(t.TempDir(), "rec.jsonl"), scenario: map[string]string{}}
	home := t.TempDir()
	t.Setenv("HOME", home)
	f.state = filepath.Join(t.TempDir(), "aggy")
	if st := os.Getenv("AGGY_TEST_STATE"); st != "" {
		f.state = st // a helper process shares its parent test's state
	}
	if rec := os.Getenv("AGGY_TEST_RECORD"); rec != "" {
		f.record = rec
	}
	t.Setenv("AGGY_HOME", f.state)
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME", "AGGY_DELEGATED", "AGGY_VENDOR"} {
		t.Setenv(v, "")
	}
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vendors {
		data, err := safefs.ReadFile(f.fakes.For(string(v)))
		if err != nil {
			t.Fatal(err)
		}
		if err := safefs.WriteFile(filepath.Join(f.bin, string(v)), data, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := s.PutVendorStatus(ctx, store.VendorStatus{Vendor: string(v), Connected: "unknown", CheckedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Close()
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.repo = repo
	withFake := func(run adapter.Runner) adapter.Runner {
		return func(ctx context.Context, c adapter.Command) (adapter.Output, error) {
			c.Env = append(append([]string{}, c.Env...), fakecli.EnvRecord+"="+f.record, fakecli.EnvVendor+"="+filepath.Base(c.Path))
			if sc := f.scenario[filepath.Base(c.Path)]; sc != "" {
				c.Env = append(c.Env, fakecli.EnvScenario+"="+fakecli.WriteScenario(t, sc))
			}
			return run(ctx, c)
		}
	}
	f.env = borrowEnv{
		getenv: func(k string) string { return "" },
		cwd:    func() (string, error) { return repo, nil },
		lookPath: func(name string) (string, error) {
			p := filepath.Join(f.bin, name)
			if _, err := safefs.Stat(p); err != nil {
				return "", probe.ErrNotFound
			}
			return p, nil
		},
		probeRun: func(ctx context.Context, c adapter.Command) (adapter.Output, error) {
			return withFake(execrun.ExecRunner(probeTimeout))(ctx, c)
		},
		runner: func(timeout time.Duration) adapter.Runner { return withFake(headlessRunner(timeout)) },
		now:    time.Now,
		// The host's kernel is not under test here (probe.TestKernelSandbox).
		kernelSandbox: func() error { return nil },
	}
	return f
}

func (f *borrowFixture) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := borrowWith(args, f.env, &out, &errb)
	return code, out.String(), errb.String()
}

func TestBorrowCommand(t *testing.T) {
	f := newBorrowFixture(t, adapter.Grok)
	f.scenario["grok"] = `{"json":{"result":"Summary: Go 1.27 shipped.\n\u001b[31mred\u001b[0m line"}}`
	code, out, errOut := f.run(t, "news", "what shipped in go 1.27?", "--constraint", "be brief")
	if code != 0 {
		t.Fatalf("code %d, stderr %q, stdout %q", code, errOut, out)
	}
	for _, w := range []string{"news on grok (default model): ok", "Summary: Go 1.27 shipped.", `\x1b[31mred`, "Report: ", "-what-shipped-in-go-1-27.md"} {
		if !strings.Contains(out, w) {
			t.Errorf("stdout lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("raw escape reached the terminal: %q", out)
	}
	if n := strings.Count(strings.TrimSpace(out), "\n") + 1; n > 12 {
		t.Errorf("short report is %d lines", n)
	}
	// T6641: the same question again is answered from the cache.
	code, out, errOut = f.run(t, "news", "what shipped in go 1.27?", "--constraint", "be brief")
	if code != 0 || !strings.Contains(out, "cached from") || !strings.Contains(out, "Summary: Go 1.27 shipped.") || !strings.Contains(out, "Report: ") {
		t.Errorf("cached run: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if code, _, errOut = f.run(t, "image", "a cat", "--fresh"); code != 2 || !strings.Contains(errOut, "--fresh is for research and news") {
		t.Errorf("image --fresh: code %d, stderr %q", code, errOut)
	}
}

// TestBorrowCommandDelegated (T6623): aggy borrow refuses inside a
// borrow child.
func TestBorrowCommandDelegated(t *testing.T) {
	f := newBorrowFixture(t, adapter.Grok)
	f.env.getenv = func(k string) string {
		if k == "AGGY_DELEGATED" {
			return "1"
		}
		return ""
	}
	code, _, errOut := f.run(t, "news", "x")
	if code != 1 || !strings.Contains(errOut, "one hop") {
		t.Errorf("code %d stderr %q", code, errOut)
	}
}

func TestBorrowCommandRefusals(t *testing.T) {
	f := newBorrowFixture(t, adapter.Grok)
	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"edit", []string{"edit", "fix"}, 2, "edit comes in M3"},
		{"unknown kind", []string{"chat", "hi"}, 2, "unknown kind"},
		{"vendor not installed", []string{"image", "a cat", "--vendor", "codex"}, 1, "codex: not set up (run aggy setup)"},
		{"both", []string{"research", "q", "--vendor", "both"}, 1, "only one vendor supports research right now"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, _, errOut := f.run(t, tt.args...)
			if code != tt.code || !strings.Contains(errOut, tt.want) {
				t.Errorf("code %d stderr %q", code, errOut)
			}
		})
	}
	if recs := fakecli.Records(t, f.record); slices.ContainsFunc(recs, func(r fakecli.Record) bool { return r.Mode == "headless" }) {
		t.Error("a refused borrow ran a child")
	}
}

// TestBorrowAllowedVendors (#10-1): the repo's allowed_vendors and the
// routing table bind routing and --vendor alike. A refused borrow runs
// nothing.
func TestBorrowAllowedVendors(t *testing.T) {
	tests := []struct {
		name     string
		repoCfg  string
		userCfg  string
		args     []string
		wantErr  string
		wantRuns int
	}{
		{"claude only: news never routes to grok", `allowed_vendors = ["claude"]`, "",
			[]string{"news", "x"}, "grok: not in this repo's allowed_vendors", 0},
		{"claude only: --vendor grok refused", `allowed_vendors = ["claude"]`, "",
			[]string{"news", "x", "--vendor", "grok"}, "grok: not in this repo's allowed_vendors", 0},
		{"no vendor at all", `allowed_vendors = []`, "",
			[]string{"news", "x", "--vendor", "grok"}, "grok: not in this repo's allowed_vendors", 0},
		{"kind turned off in routing", "", "[routing]\nnews = []\n",
			[]string{"news", "x"}, "[routing] news lists no vendor", 0},
		{"grok allowed", `allowed_vendors = ["grok"]`, "", []string{"news", "x"}, "", 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newBorrowFixture(t, adapter.Grok, adapter.Claude)
			f.scenario["grok"] = `{"json":{"result":"ok"}}`
			if tt.repoCfg != "" {
				mustWrite(t, filepath.Join(f.repo, ".aggy", "config.toml"), tt.repoCfg+"\n", 0o600)
			}
			if tt.userCfg != "" {
				mustWrite(t, filepath.Join(f.state, "config.toml"), tt.userCfg, 0o600)
			}
			code, _, errOut := f.run(t, tt.args...)
			if tt.wantErr != "" && (code == 0 || !strings.Contains(errOut, tt.wantErr)) {
				t.Errorf("code %d stderr %q", code, errOut)
			}
			if tt.wantErr == "" && code != 0 {
				t.Errorf("code %d stderr %q", code, errOut)
			}
			runs := 0
			for _, r := range fakecli.Records(t, f.record) {
				if r.Mode == "headless" {
					runs++
				}
			}
			if runs != tt.wantRuns {
				t.Errorf("runs = %d, want %d", runs, tt.wantRuns)
			}
		})
	}
}

// TestBorrowHelperProcess is the aggy borrow that TestBorrowSignalKillsChild
// signals. It runs only as that test's helper.
func TestBorrowHelperProcess(t *testing.T) {
	if os.Getenv("AGGY_TEST_BORROW_HELPER") != "1" {
		t.Skip("helper process only")
	}
	f := newBorrowFixture(t, adapter.Grok)
	f.scenario["grok"] = `{"sleep_ms":60000,"json":{"result":"late"}}`
	code, out, errOut := f.run(t, "news", "x")
	t.Logf("code %d\n%s\n%s", code, out, errOut)
}

// TestBorrowSignalKillsChild (#10-2): SIGTERM, SIGINT, or SIGHUP to aggy
// borrow kills the child, records the borrow as killed (interrupted),
// and removes the child's temp dir. Two signals close together (R-5,
// C17-7) still kill the child before aggy can end.
func TestBorrowSignalKillsChild(t *testing.T) {
	if os.Getenv("AGGY_TEST_BORROW_HELPER") != "" {
		t.Skip()
	}
	tests := []struct {
		name string
		sigs []syscall.Signal
	}{
		{"terminated", []syscall.Signal{syscall.SIGTERM}},
		{"interrupt", []syscall.Signal{syscall.SIGINT}},
		{"hangup", []syscall.Signal{syscall.SIGHUP}},
		{"hangup then terminated", []syscall.Signal{syscall.SIGHUP, syscall.SIGTERM}},
		{"two terminated", []syscall.Signal{syscall.SIGTERM, syscall.SIGTERM}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "aggy")
			record := filepath.Join(t.TempDir(), "rec.jsonl")
			cmd := exec.Command(os.Args[0], "-test.run=^TestBorrowHelperProcess$", "-test.v")
			cmd.Env = append(os.Environ(), "AGGY_TEST_BORROW_HELPER=1", "AGGY_TEST_STATE="+state,
				"AGGY_TEST_RECORD="+record, "HOME="+t.TempDir())
			var out bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill() }()
			var child fakecli.Record
			deadline := time.Now().Add(60 * time.Second)
			for child.PID == 0 {
				if time.Now().After(deadline) {
					t.Fatalf("the child never started:\n%s", out.String())
				}
				time.Sleep(50 * time.Millisecond)
				for _, r := range fakecli.Records(t, record) {
					if r.Mode == "headless" {
						child = r
					}
				}
			}
			for _, sig := range tt.sigs {
				// A second signal may find aggy already gone.
				_ = cmd.Process.Signal(sig)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case werr := <-done:
				t.Logf("aggy borrow exited: %v", werr)
			case <-time.After(15 * time.Second):
				t.Fatalf("aggy borrow did not exit after %v:\n%s", tt.sigs, out.String())
			}
			gone := time.Now().Add(5 * time.Second)
			for syscall.Kill(child.PID, 0) == nil {
				if time.Now().After(gone) {
					_ = syscall.Kill(child.PID, syscall.SIGKILL)
					t.Fatalf("child %d outlived aggy borrow", child.PID)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if len(tt.sigs) > 1 {
				return // the second signal may end aggy before its cleanup
			}
			if _, err := os.Stat(child.Cwd); !os.IsNotExist(err) {
				t.Errorf("temp dir %s left behind: %v", child.Cwd, err)
			}
			t.Setenv("AGGY_HOME", state)
			s, err := store.OpenDefault(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			var status, outcome string
			if err := s.DB().QueryRow(`SELECT status, outcome FROM borrows`).Scan(&status, &outcome); err != nil {
				t.Fatal(err)
			}
			if status != "killed" || outcome != "interrupted" {
				t.Errorf("borrow %s/%s, want killed/interrupted:\n%s", status, outcome, out.String())
			}
		})
	}
}

// TestBorrowRepoRoot (X-2): run from a subdirectory, aggy borrow reads
// the repo config at the work tree root, never just the cwd.
func TestBorrowRepoRoot(t *testing.T) {
	f := newBorrowFixture(t, adapter.Grok)
	mustWrite(t, filepath.Join(f.repo, ".git", "HEAD"), "ref: refs/heads/main\n", 0o600)
	mustWrite(t, filepath.Join(f.repo, ".aggy", "config.toml"), "allowed_vendors = [\"claude\"]\n", 0o600)
	sub := filepath.Join(f.repo, "pkg", "deep")
	mustWrite(t, filepath.Join(sub, "x.go"), "package deep\n", 0o600)
	f.env.cwd = func() (string, error) { return sub, nil }
	code, _, errOut := f.run(t, "news", "x")
	if code == 0 || !strings.Contains(errOut, "grok: not in this repo's allowed_vendors") {
		t.Errorf("code %d stderr %q", code, errOut)
	}
}
