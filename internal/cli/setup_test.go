package cli

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/execrun"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// statusArgs is the sign-in status command the tests give an adapter.
// No vendor's is verified, so the real adapters have none; the fakes
// answer it through a scenario reply.
var statusArgs = []string{"fake-auth-status"}

// withCheck is an adapter with a sign-in status check: exit 0 is signed
// in, exit 1 signed out, anything else unknown.
type withCheck struct{ adapter.Adapter }

func (w withCheck) SignIn() adapter.SignIn {
	si := w.Adapter.SignIn()
	si.Check = &adapter.SignInCheck{Args: statusArgs, Parse: func(o adapter.Output) adapter.SignInStatus {
		switch o.ExitCode {
		case 0:
			return adapter.SignedIn
		case 1:
			return adapter.SignedOut
		}
		return adapter.SignInUnknown
	}}
	return si
}

// fakeWorld is a checkEnv on the fake CLIs. scenario returns the
// scenario JSON for a run of vendor; n counts the sign-in checks of that
// vendor so far.
type fakeWorld struct {
	t        *testing.T
	fakes    fakecli.Fakes
	record   string
	mu       sync.Mutex
	checks   map[string]int
	environ  []string
	scenario func(vendor string, checks int) string
}

func newFakeWorld(t *testing.T, fakes fakecli.Fakes) *fakeWorld {
	return &fakeWorld{t: t, fakes: fakes, record: filepath.Join(t.TempDir(), "rec.jsonl"), checks: map[string]int{}}
}

func (w *fakeWorld) runner() adapter.Runner {
	real := execrun.ExecRunner(20 * time.Second)
	return func(ctx context.Context, c adapter.Command) (adapter.Output, error) {
		vendor := strings.TrimPrefix(filepath.Base(c.Path), "fake-")
		w.mu.Lock()
		n := w.checks[vendor]
		if strings.Join(c.Args, " ") == strings.Join(statusArgs, " ") {
			w.checks[vendor]++
		}
		w.mu.Unlock()
		c.Env = append(c.Env, fakecli.EnvRecord+"="+w.record)
		if w.scenario != nil {
			if sc := w.scenario(vendor, n); sc != "" {
				c.Env = append(c.Env, fakecli.EnvScenario+"="+fakecli.WriteScenario(w.t, sc))
			}
		}
		return real(ctx, c)
	}
}

func (w *fakeWorld) env(home, input string, tty bool, adapters ...adapter.Adapter) checkEnv {
	if adapters == nil {
		adapters = []adapter.Adapter{
			claude.Adapter{Path: w.fakes.Claude},
			codex.Adapter{Path: w.fakes.Codex},
			grok.Adapter{Path: w.fakes.Grok},
		}
	}
	return checkEnv{
		adapters: adapters, run: w.runner(), in: strings.NewReader(input), tty: tty, home: home,
		environ: w.environ,
		now:     func() time.Time { return time.UnixMilli(1_800_000_000_000) },
	}
}

func vendorRows(t *testing.T) map[string]store.VendorStatus {
	t.Helper()
	s, err := store.OpenDefault(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	rows, err := s.VendorStatuses(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]store.VendorStatus{}
	for _, r := range rows {
		out[r.Vendor] = r
	}
	return out
}

// signedOutOnce answers the status check "signed out" on the first check
// and "signed in" after.
func signedOutOnce(_ string, n int) string {
	code := 1
	if n > 0 {
		code = 0
	}
	return `{"replies":[{"args":["fake-auth-status"],"exit_code":` + string(rune('0'+code)) + `}]}`
}

func TestSetup(t *testing.T) {
	fakes := fakecli.Build(t)
	type row struct{ connected, version string }
	tests := []struct {
		name     string
		args     []string
		input    string
		tty      bool
		check    bool // give the adapters a sign-in check
		scenario func(string, int) string
		want     map[string]row // nil: no row
		out      []string
		notOut   []string
	}{
		{
			name:   "no terminal: probe and record all, no prompts",
			want:   map[string]row{"claude": {"unknown", "2.1.311"}, "codex": {"unknown", "0.164.0"}, "grok": {"unknown", "1.0.70"}},
			out:    []string{"no verified way to ask Codex", "run `codex login`", "later release"},
			notOut: []string{"[Y/n]", "Press Enter"},
		},
		{
			name: "--yes on a terminal does not prompt",
			args: []string{"--yes"}, tty: true,
			want:   map[string]row{"claude": {"unknown", "2.1.311"}, "codex": {"unknown", "0.164.0"}, "grok": {"unknown", "1.0.70"}},
			notOut: []string{"[Y/n]"},
		},
		{
			name: "--only",
			args: []string{"--only", "grok,codex"},
			want: map[string]row{"codex": {"unknown", "0.164.0"}, "grok": {"unknown", "1.0.70"}},
		},
		{
			name:  "signed in and signed out, no terminal",
			check: true,
			scenario: func(v string, _ int) string {
				if v == "grok" {
					return `{"replies":[{"args":["fake-auth-status"],"exit_code":1}]}`
				}
				return `{"replies":[{"args":["fake-auth-status"],"exit_code":0}]}`
			},
			want:   map[string]row{"claude": {"connected", "2.1.311"}, "codex": {"connected", "0.164.0"}, "grok": {"not_connected", "1.0.70"}},
			out:    []string{"Sign in with Grok Build's own flow: run `grok login`"},
			notOut: []string{"Press Enter"},
		},
		{
			name: "interactive: skip one, sign in and re-check one",
			tty:  true, check: true,
			input:    "n\ny\n\n\n",
			scenario: signedOutOnce,
			want:     map[string]row{"codex": {"connected", "0.164.0"}, "grok": {"not_connected", "1.0.70"}},
			out:      []string{"Check Claude Code now? [Y/n]", "skipped", "sign-in: not signed in", "sign-in: signed in", "recorded: connected, 0.164.0"},
		},
		{
			name: "interactive: skip the re-check",
			tty:  true, check: true,
			input:    "y\ns\nn\nn\n",
			scenario: signedOutOnce,
			want:     map[string]row{"claude": {"not_connected", "2.1.311"}},
		},
		{
			name:  "interactive: input ends",
			tty:   true,
			input: "y\n",
			want:  map[string]row{"claude": {"unknown", "2.1.311"}},
		},
		{
			name: "odd versions are warned about and escaped",
			scenario: func(v string, _ int) string {
				switch v {
				case "codex":
					return `{"version_output":"codex-cli 0.200.0-beta.1\n"}`
				case "grok":
					return `{"version_output":"grok build \u001b]0;owned\u0007\n"}`
				}
				return ""
			},
			want: map[string]row{"claude": {"unknown", "2.1.311"}, "codex": {"unknown", "0.200.0-beta.1"}, "grok": {"unknown", ""}},
			out:  []string{"codex 0.200.0-beta.1 is outside the versions", "version unreadable", "cannot read the CLI version"},
		},
		{
			name:  "sign-in check fails: unknown",
			check: true,
			scenario: func(string, int) string {
				return `{"replies":[{"args":["fake-auth-status"],"exit_code":7,"stderr":"\u001b[2Jboom"}]}`
			},
			want: map[string]row{"claude": {"unknown", "2.1.311"}, "codex": {"unknown", "0.164.0"}, "grok": {"unknown", "1.0.70"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := isolate(t)
			w := newFakeWorld(t, fakes)
			w.scenario = tt.scenario
			var as []adapter.Adapter
			if tt.check {
				as = []adapter.Adapter{
					withCheck{claude.Adapter{Path: fakes.Claude}},
					withCheck{codex.Adapter{Path: fakes.Codex}},
					withCheck{grok.Adapter{Path: fakes.Grok}},
				}
			}
			var stdout, stderr bytes.Buffer
			code := setupWith(tt.args, w.env(home, tt.input, tt.tty, as...), &stdout, &stderr)
			out := stdout.String()
			if code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, stderr.String())
			}
			rows := vendorRows(t)
			if len(rows) != len(tt.want) {
				t.Errorf("rows = %+v, want %+v", rows, tt.want)
			}
			for v, r := range tt.want {
				got := rows[v]
				if got.Connected != r.connected || got.Version != r.version || !got.CheckedAt.Equal(time.UnixMilli(1_800_000_000_000)) {
					t.Errorf("%s = %+v, want %+v", v, got, r)
				}
			}
			for _, s := range tt.out {
				if !strings.Contains(out, s) {
					t.Errorf("output lacks %q:\n%s", s, out)
				}
			}
			for _, s := range tt.notOut {
				if strings.Contains(out, s) {
					t.Errorf("output has %q:\n%s", s, out)
				}
			}
			if strings.ContainsAny(out+stderr.String(), "\x1b\x07") {
				t.Errorf("raw control characters reached the terminal:\n%q", out)
			}
		})
	}
}

// TestSetupMissingCLI: a CLI not on PATH is recorded as not connected
// with no version, and nothing runs.
func TestSetupMissingCLI(t *testing.T) {
	home := isolate(t)
	t.Setenv("PATH", t.TempDir())
	w := newFakeWorld(t, fakecli.Fakes{})
	var stdout, stderr bytes.Buffer
	env := w.env(home, "", false, claude.New(), codex.New(), grok.New())
	if code := setupWith(nil, env, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, v := range []string{"claude", "codex", "grok"} {
		if r := vendorRows(t)[v]; r.Connected != "not_connected" || r.Version != "" {
			t.Errorf("%s = %+v", v, r)
		}
	}
	if !strings.Contains(stdout.String(), "Install Codex, then run aggy setup again") {
		t.Errorf("output:\n%s", stdout.String())
	}
	if len(fakecli.Records(t, w.record)) != 0 {
		t.Error("a CLI ran")
	}
}

// TestSetupRerun: a second setup replaces a vendor's row, and a skipped
// vendor keeps its old one.
func TestSetupRerun(t *testing.T) {
	fakes := fakecli.Build(t)
	home := isolate(t)
	w := newFakeWorld(t, fakes)
	var out bytes.Buffer
	if code := setupWith(nil, w.env(home, "", false), &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	w.scenario = func(v string, _ int) string { return `{"version":"2.1.312"}` }
	if code := setupWith([]string{"--only", "claude"}, w.env(home, "", false), &out, &out); code != 0 {
		t.Fatal(out.String())
	}
	rows := vendorRows(t)
	if rows["claude"].Version != "2.1.312" || rows["codex"].Version != "0.164.0" || len(rows) != 3 {
		t.Errorf("rows = %+v", rows)
	}
}

// TestSetupAPIKeyWarning (T6610): a set API-key variable is named, with
// the vendor it concerns, and its value never appears anywhere: not in
// the output and not in the store.
func TestSetupAPIKeyWarning(t *testing.T) {
	fakes := fakecli.Build(t)
	home := isolate(t)
	w := newFakeWorld(t, fakes)
	const secret = "sk-SETUPSECRET-0123456789abcdef"
	w.environ = []string{"OPENAI_API_KEY=" + secret, "ANTHROPIC_API_KEY=", "XAI_API_KEY=" + secret}
	var stdout, stderr bytes.Buffer
	if code := setupWith(nil, w.env(home, "", false), &stdout, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	out := stdout.String()
	codexPart := out[strings.Index(out, "\nCodex\n"):strings.Index(out, "\nGrok Build\n")]
	if !strings.Contains(codexPart, "warning: OPENAI_API_KEY is set: the CLI may bill the API instead of the subscription") {
		t.Errorf("no Codex warning:\n%s", out)
	}
	if strings.Contains(out, "ANTHROPIC_API_KEY") || !strings.Contains(out, "XAI_API_KEY is set") {
		t.Errorf("wrong warnings:\n%s", out)
	}
	path, _ := store.DefaultPath()
	db, err := safefsReadAll(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out+stderr.String(), "SETUPSECRET") || bytes.Contains(db, []byte("SETUPSECRET")) {
		t.Error("the value leaked")
	}
}

// TestSetupRoutes (T6611): base-URL variables and config-file routes are
// shown under their vendor by name and key, never by value.
func TestSetupRoutes(t *testing.T) {
	fakes := fakecli.Build(t)
	home := isolate(t)
	const secret = "ROUTESECRET"
	if err := safefs.MkdirAll(filepath.Join(home, ".grok"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := safefs.WriteFile(filepath.Join(home, ".grok", "config.toml"), []byte("[toolset.image_gen]\nenv_key = \""+secret+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	w := newFakeWorld(t, fakes)
	w.environ = []string{"HOME=" + home, "ANTHROPIC_BASE_URL=http://" + secret}
	var stdout, stderr bytes.Buffer
	if code := setupWith(nil, w.env(home, "", false), &stdout, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	out := stdout.String()
	claudePart := out[strings.Index(out, "\nClaude Code\n"):strings.Index(out, "\nCodex\n")]
	grokPart := out[strings.Index(out, "\nGrok Build\n"):]
	if !strings.Contains(claudePart, "warning: ANTHROPIC_BASE_URL is set") ||
		!strings.Contains(grokPart, "warning: ~/.grok/config.toml: toolset.image_gen.env_key is set") {
		t.Errorf("output:\n%s", out)
	}
	if strings.Contains(out, secret) {
		t.Error("a value leaked")
	}
}

func TestSetupArgs(t *testing.T) {
	isolate(t)
	for _, tt := range []struct {
		args []string
		code int
	}{
		{[]string{"--help"}, 0},
		{[]string{"--only", "gemini"}, 2},
		{[]string{"extra"}, 2},
		{[]string{"--bogus"}, 2},
	} {
		if code, _, _ := run(append([]string{"setup"}, tt.args...)...); code != tt.code {
			t.Errorf("%v: exit %d, want %d", tt.args, code, tt.code)
		}
	}
}

// safefsReadAll reads the database and its WAL, as they are on disk.
func safefsReadAll(path string) ([]byte, error) {
	var all []byte
	for _, p := range []string{path, path + "-wal"} {
		b, err := safefs.ReadFile(p)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		all = append(all, b...)
	}
	return all, nil
}
