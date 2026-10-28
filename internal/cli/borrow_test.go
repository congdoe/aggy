package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"strings"
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
			if got.kind != tt.want.kind || got.task != tt.want.task || got.vendor != tt.want.vendor || got.help != tt.want.help ||
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
}

func newBorrowFixture(t *testing.T, vendors ...adapter.Vendor) *borrowFixture {
	t.Helper()
	f := &borrowFixture{fakes: fakecli.Build(t), bin: t.TempDir(), record: filepath.Join(t.TempDir(), "rec.jsonl"), scenario: map[string]string{}}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("AGGY_HOME", filepath.Join(t.TempDir(), "aggy"))
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
	for _, w := range []string{"news on grok (default model): ok", "Summary: Go 1.27 shipped.", `\x1b[31mred`, "Full report: ", "/inbox/"} {
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
