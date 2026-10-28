package cli

import (
	"bytes"
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/devfix"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// doctorSecret is a value no doctor output may hold.
const doctorSecret = "sk-DOCTORSECRET0123456789"

func mustWrite(t *testing.T, p, content string, mode uint32) {
	t.Helper()
	if err := safefs.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := safefs.WriteFile(p, []byte(content), fsMode(mode)); err != nil {
		t.Fatal(err)
	}
}

func TestDoctor(t *testing.T) {
	fakes := fakecli.Build(t)
	tests := []struct {
		name    string
		prepare func(t *testing.T, home string, w *fakeWorld) []adapter.Adapter // nil adapters: the three fakes
		code    int
		want    []string // substrings of the output
		notWant []string
	}{
		{
			name: "all clean",
			want: []string{"Claude Code\n", "info    cli-found: ", "version 2.1.311", "info    sign-in-unknown: aggy has no verified way to ask Codex", "no-index: aggy has not changed any config", "0 errors, 0 warnings"},
		},
		{
			name: "API keys, base URLs, and config routes",
			prepare: func(t *testing.T, home string, w *fakeWorld) []adapter.Adapter {
				w.environ = []string{"HOME=" + home, "XAI_API_KEY=" + doctorSecret, "OPENAI_BASE_URL=http://" + doctorSecret}
				mustWrite(t, filepath.Join(home, ".claude", "settings.json"), `{"env":{"ANTHROPIC_API_KEY":"`+doctorSecret+`"}}`, 0o600)
				mustWrite(t, filepath.Join(home, ".grok", "config.toml"), "[endpoints]\nxai_api_base_url = \""+doctorSecret+"\"\n", 0o600)
				return nil
			},
			want: []string{
				"warning api-key-env: XAI_API_KEY is set: the CLI may bill the API instead of the subscription",
				"warning base-url-env: OPENAI_BASE_URL is set",
				"warning route-config: ~/.claude/settings.json: env.ANTHROPIC_API_KEY is set",
				"warning route-config: ~/.grok/config.toml: endpoints.xai_api_base_url is set",
				"General\n  warning base-url-env: OPENAI_BASE_URL is set: Codex does not read it",
				"0 errors, 4 warnings",
			},
			notWant: []string{"DOCTORSECRET"},
		},
		{
			name: "untested and unreadable versions",
			prepare: func(t *testing.T, _ string, w *fakeWorld) []adapter.Adapter {
				w.scenario = func(v string, _ int) string {
					switch v {
					case "claude":
						return `{"version":"3.0.0"}`
					case "grok":
						return `{"version_output":"grok \u001b[31mnightly\n"}`
					}
					return ""
				}
				return nil
			},
			want:    []string{"warning version-untested: claude 3.0.0 is outside the versions aggy was tested on (2.1.311 to 2.1.312)", "version unreadable", "cannot read the CLI version"},
			notWant: []string{"\x1b"},
		},
		{
			name: "signed out",
			prepare: func(t *testing.T, _ string, w *fakeWorld) []adapter.Adapter {
				w.scenario = func(string, int) string { return `{"replies":[{"args":["fake-auth-status"],"exit_code":1}]}` }
				return []adapter.Adapter{withCheck{codex.Adapter{Path: w.fakes.Codex}}}
			},
			want: []string{"warning signed-out: Codex is not signed in: run `codex login`"},
		},
		{
			name: "a CLI that fails its version check",
			prepare: func(t *testing.T, _ string, w *fakeWorld) []adapter.Adapter {
				p := filepath.Join(t.TempDir(), "claude")
				mustWrite(t, p, "not a program", 0o644)
				return []adapter.Adapter{claude.Adapter{Path: p}, grok.Adapter{Path: w.fakes.Grok}}
			},
			code: 1,
			want: []string{"error   cli-probe-failed: Claude Code did not answer its version check", "1 error,"},
		},
		{
			name: "no CLI installed",
			prepare: func(t *testing.T, _ string, _ *fakeWorld) []adapter.Adapter {
				t.Setenv("PATH", t.TempDir())
				return []adapter.Adapter{claude.New(), codex.New(), grok.New()}
			},
			code: 1,
			want: []string{"info    cli-missing: Codex is not installed", "error   no-cli: no vendor CLI is installed"},
		},
		{
			name: "config drift",
			prepare: func(t *testing.T, home string, _ *fakeWorld) []adapter.Adapter {
				a, b := filepath.Join(home, ".codex", "config.toml"), filepath.Join(home, ".grok", "x.toml")
				mustWrite(t, a, "a = 1\n", 0o600)
				mustWrite(t, b, "b = 1\n", 0o600)
				applyAll(t, "run-1",
					patch.Patch{File: a, ExpectedHash: patch.Hash([]byte("a = 1\n")), Transform: appendOnce("x = 2\n")},
					patch.Patch{File: b, ExpectedHash: patch.Hash([]byte("b = 1\n")), Transform: appendOnce("y = 2\n")})
				mustWrite(t, a, "a = 1\nx = 3\n", 0o600)
				return nil
			},
			want:    []string{"warning config-drift: ~/.codex/config.toml: changed after aggy wrote it", "0 errors, 1 warning"},
			notWant: []string{"x.toml", "x = 3"},
		},
		{
			name: "no drift",
			prepare: func(t *testing.T, home string, _ *fakeWorld) []adapter.Adapter {
				a := filepath.Join(home, ".codex", "config.toml")
				mustWrite(t, a, "a = 1\n", 0o600)
				applyAll(t, "run-1", patch.Patch{File: a, ExpectedHash: patch.Hash([]byte("a = 1\n")), Transform: appendOnce("x = 2\n")})
				return nil
			},
			want: []string{"info    no-drift: 1 change aggy made can be undone cleanly by aggy unwire"},
		},
		{
			name: "a recorded change naming a credential file",
			prepare: func(t *testing.T, home string, _ *fakeWorld) []adapter.Adapter {
				s, err := store.OpenDefault(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = s.Close() }()
				if err := s.InsertChange(context.Background(), store.Change{
					ChangeID: "c1", File: filepath.Join(home, ".codex", "auth.json"),
					HashBefore: patch.Hash(nil), HashAfter: patch.Hash([]byte("x")),
					Status: store.ChangeApplied, PlannedAt: time.Now(),
				}); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			code: 1,
			want: []string{"error   drift-unchecked: ~/.codex/auth.json: change c1 names a vendor credential file, which aggy never reads"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home := isolate(t)
			w := newFakeWorld(t, fakes)
			w.environ = []string{"HOME=" + home}
			var as []adapter.Adapter
			if tt.prepare != nil {
				as = tt.prepare(t, home, w)
			}
			var stdout, stderr bytes.Buffer
			code := doctorWith(nil, w.env(home, "", false, as...), &stdout, &stderr)
			out := stdout.String()
			if code != tt.code {
				t.Errorf("exit %d, want %d\n%s%s", code, tt.code, out, stderr.String())
			}
			for _, s := range tt.want {
				if !strings.Contains(out, s) {
					t.Errorf("output lacks %q:\n%s", s, out)
				}
			}
			for _, s := range tt.notWant {
				if strings.Contains(out, s) {
					t.Errorf("output has %q:\n%s", s, out)
				}
			}
		})
	}
}

// TestDoctorWritesNothing: doctor with no index creates none.
func TestDoctorWritesNothing(t *testing.T) {
	home := isolate(t)
	w := newFakeWorld(t, fakecli.Fakes{})
	t.Setenv("PATH", t.TempDir())
	var out bytes.Buffer
	doctorWith(nil, w.env(home, "", false, claude.New()), &out, &out)
	path, _ := store.DefaultPath()
	if _, err := safefs.Stat(path); err == nil {
		t.Error("doctor created the index")
	}
}

// TestDoctorReport: --report keeps versions and codes and drops what
// identifies the person: home path, user and host names, emails, token-
// shaped text, and every environment value.
func TestDoctorReport(t *testing.T) {
	fakes := fakecli.Build(t)
	home := isolate(t)
	w := newFakeWorld(t, fakes)
	host := filepath.Join(t.TempDir(), "alices-macbook")
	w.environ = []string{"HOME=" + home, "OPENAI_API_KEY=" + doctorSecret, "ANTHROPIC_BASE_URL=http://alice@example.org/" + doctorSecret}
	// A changed file outside home, under a folder named like the host,
	// and one under home whose name holds an email and a token.
	outside := filepath.Join(host, "cfg.toml")
	inside := filepath.Join(home, "alice@corp.io-sk-abcdefghijklmnopqrstuv.toml")
	mustWrite(t, outside, "a\n", 0o600)
	mustWrite(t, inside, "b\n", 0o600)
	applyAll(t, "run-1",
		patch.Patch{File: outside, ExpectedHash: patch.Hash([]byte("a\n")), Transform: appendOnce("x\n")},
		patch.Patch{File: inside, ExpectedHash: patch.Hash([]byte("b\n")), Transform: appendOnce("y\n")})
	mustWrite(t, outside, "edited\n", 0o600)
	mustWrite(t, inside, "edited\n", 0o600)

	env := w.env(home, "", false)
	env.identity = &devfix.Identity{Home: home, User: "alice", Host: "alices-macbook"}
	var stdout, stderr bytes.Buffer
	code := doctorWith([]string{"--report"}, env, &stdout, &stderr)
	out := stdout.String()
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, stderr.String())
	}
	for _, s := range []string{"aggy doctor report\n", "aggy: ", "os: ", "claude: 2.1.311\n", "codex: 0.164.0\n", "grok: 1.0.70\n",
		"warning api-key-env (codex): OPENAI_API_KEY is set", "warning base-url-env (claude): ANTHROPIC_BASE_URL is set",
		"warning config-drift: <path>/cfg.toml: changed", "info sign-in-unknown (grok)"} {
		if !strings.Contains(out, s) {
			t.Errorf("report lacks %q:\n%s", s, out)
		}
	}
	for _, s := range []string{"DOCTORSECRET", home, "alices-macbook", "alice@", "corp.io", "sk-abcdefghij", "example.org", "edited", "\x1b"} {
		if strings.Contains(out, s) {
			t.Errorf("report has %q:\n%s", s, out)
		}
	}
}

func TestDoctorArgs(t *testing.T) {
	isolate(t)
	for _, tt := range []struct {
		args []string
		code int
	}{
		{[]string{"--help"}, 0},
		{[]string{"extra"}, 2},
		{[]string{"--bogus"}, 2},
	} {
		if code, _, _ := run(append([]string{"doctor"}, tt.args...)...); code != tt.code {
			t.Errorf("%v: exit %d, want %d", tt.args, code, tt.code)
		}
	}
}

func fsMode(m uint32) fs.FileMode { return fs.FileMode(m) }

// TestReportScrub (T6612 review 2): the user name in paths outside home,
// and paths outside home themselves, never reach a report.
func TestReportScrub(t *testing.T) {
	id := devfix.Identity{Home: "/home/alice", User: "alice"}
	tests := []struct{ in, want string }{
		{"/etc/profiles/per-user/alice/bin/claude, version 2.1.311", "<path>/claude, version 2.1.311"},
		{"/home/alice/bin/claude, version 1", "~/bin/claude, version 1"},
		{"probe: /home/alice/x/codex --version exited 1", "probe: ~/x/codex --version exited 1"},
		{"/home/alice", "~"},
		{"/home/alicebob/x", "<path>/x"},
		{"~/.grok/config.toml: endpoints.xai_api_base_url is set", "~/.grok/config.toml: endpoints.xai_api_base_url is set"},
		{"run `claude`, then type `/login`", "run `claude`, then type `/login`"},
		{"cc-alice: set; alice.smith and Alice_x and malice", "cc-fixture-user: set; fixture-user.smith and Alice_x and malice"},
		{"/tmp/cc-alice/settings.json: env.X is set", "<path>/settings.json: env.X is set"},
		{"alice-alice", "fixture-user-fixture-user"},
	}
	for _, tt := range tests {
		if got := reportScrub(tt.in, id); got != tt.want {
			t.Errorf("reportScrub(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	// T6612 re-review A: paths with spaces are scrubbed at the source,
	// through a finding's Path, not by the regex.
	for _, tt := range []struct {
		f    checkup.Finding
		want string
	}{
		{checkup.Finding{Path: "/mnt/c/Users/John Doe/AppData/claude", Message: "version 2.1.311"}, "<path>/claude: version 2.1.311"},
		{checkup.Finding{Path: "/Volumes/Acme Corp Clients/cfg/settings.json", Message: "env.X is set"}, "<path>/settings.json: env.X is set"},
		{checkup.Finding{Path: "/home/alice/My Files/x.toml", Message: "changed"}, "~/My Files/x.toml: changed"},
		{checkup.Finding{Message: "Codex did not answer its version check", Detail: "probe: /mnt/c/Users/John Doe/codex --version exited 1"},
			"Codex did not answer its version check (details left out of the report)"},
	} {
		if got := reportScrub(tt.f.ReportText(id.Home), id); got != tt.want {
			t.Errorf("report of %+v = %q, want %q", tt.f, got, tt.want)
		}
	}
	// Generic and short names are left alone.
	for _, u := range []string{"root", "user", "al"} {
		in := "the " + u + " said"
		if got := reportScrub(in, devfix.Identity{User: u}); got != in {
			t.Errorf("user %q: %q became %q", u, in, got)
		}
	}
}

// TestDoctorReportNonHomePaths (T6612 review 2): a CLI installed under a
// per-user profile and a relocated vendor home named after the user.
func TestDoctorReportNonHomePaths(t *testing.T) {
	fakes := fakecli.Build(t)
	home := isolate(t)
	w := newFakeWorld(t, fakes)
	profile := filepath.Join(t.TempDir(), "per-user", "zelda", "bin")
	bin := filepath.Join(profile, "claude")
	b, err := safefs.ReadFile(fakes.Claude)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, bin, string(b), 0o755)
	cc := filepath.Join(t.TempDir(), "cc-zelda")
	mustWrite(t, filepath.Join(cc, "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"u"}}`, 0o600)
	w.environ = []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + cc}
	env := w.env(home, "", false, claude.Adapter{Path: bin})
	env.identity = &devfix.Identity{Home: home, User: "zelda", Host: "h"}
	var stdout, stderr bytes.Buffer
	doctorWith([]string{"--report"}, env, &stdout, &stderr)
	out := stdout.String()
	if strings.Contains(strings.ToLower(out), "zelda") {
		t.Errorf("the user name leaked:\n%s", out)
	}
	for _, s := range []string{"<path>/claude: version 2.1.311", "<path>/settings.json: env.ANTHROPIC_BASE_URL is set"} {
		if !strings.Contains(out, s) {
			t.Errorf("report lacks %q:\n%s", s, out)
		}
	}
}

// TestDoctorReportSpacedPaths (T6612 re-review A): a CLI under a path
// with spaces, outside home, and a probe error naming it.
func TestDoctorReportSpacedPaths(t *testing.T) {
	fakes := fakecli.Build(t)
	home := isolate(t)
	w := newFakeWorld(t, fakes)
	dir := filepath.Join(t.TempDir(), "Users", "John Doe", "AppData")
	b, err := safefs.ReadFile(fakes.Claude)
	if err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(dir, "claude")
	mustWrite(t, good, string(b), 0o755)
	broken := filepath.Join(dir, "codex")
	mustWrite(t, broken, "not a program", 0o644)
	w.environ = []string{"HOME=" + home}
	env := w.env(home, "", false, claude.Adapter{Path: good}, codex.Adapter{Path: broken})
	env.identity = &devfix.Identity{Home: home, User: "jdoe", Host: "h"}
	var stdout, stderr bytes.Buffer
	doctorWith([]string{"--report"}, env, &stdout, &stderr)
	out := stdout.String()
	for _, s := range []string{"John", "Doe", "AppData"} {
		if strings.Contains(out, s) {
			t.Errorf("report has %q:\n%s", s, out)
		}
	}
	for _, s := range []string{"cli-found (claude): <path>/claude: version 2.1.311", "cli-probe-failed (codex): Codex did not answer its version check (details left out of the report)"} {
		if !strings.Contains(out, s) {
			t.Errorf("report lacks %q:\n%s", s, out)
		}
	}
}
