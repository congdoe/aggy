package probe

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
	"github.com/congdoe/aggy/internal/store"
)

type fixture struct {
	fakes  fakecli.Fakes
	record string
	store  *store.Store
}

func setup(t *testing.T) fixture {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	// The runner passes these to the CLI: never let a real one through.
	for _, v := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "GROK_HOME"} {
		t.Setenv(v, "")
	}
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return fixture{fakes: fakecli.Build(t), record: filepath.Join(t.TempDir(), "rec.jsonl"), store: s}
}

// runner is the real ExecRunner with the fake's scenario and record file
// passed through the scrubbed environment.
func (f fixture) runner(t *testing.T, scenario string, timeout time.Duration) adapter.Runner {
	t.Helper()
	extra := []string{fakecli.EnvRecord + "=" + f.record}
	if scenario != "" {
		extra = append(extra, fakecli.EnvScenario+"="+fakecli.WriteScenario(t, scenario))
	}
	real := ExecRunner(timeout)
	return func(ctx context.Context, c adapter.Command) (adapter.Output, error) {
		c.Env = append(c.Env, extra...)
		return real(ctx, c)
	}
}

func TestDetectFakes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	want := map[adapter.Vendor]struct {
		version string
		caps    []string
	}{
		adapter.Claude: {"2.1.311", []string{CapHeadless, CapJSONOutput, CapSessionID, CapResume, CapForkSession, CapPermissionMode}},
		adapter.Grok: {"1.0.70", []string{CapHeadless, CapJSONOutput, CapSessionID, CapResume, CapForkSession,
			CapPermissionMode, CapNoAltScreen, CapTools, CapDisallowTools, CapMaxTurns}},
		adapter.Codex: {"0.164.0", []string{CapHeadless, CapResume, CapNoAltScreen, CapConfigOverride,
			CapJSONOutput, CapSandbox, CapDisable, CapImageInput}},
	}
	run := f.runner(t, "", 10*time.Second)
	for v, w := range want {
		p, err := Detect(ctx, run, v, f.fakes.For(string(v)))
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		if p.Version != w.version || !slices.Equal(p.Capabilities, w.caps) {
			t.Errorf("%s: version %q caps %v, want %q %v", v, p.Version, p.Capabilities, w.version, w.caps)
		}
	}
}

func TestProbeCachesPerPathAndVersion(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	path := f.fakes.Grok
	pr := &Prober{Store: f.store, Run: f.runner(t, "", 10*time.Second)}

	first, cached, err := pr.Probe(ctx, adapter.Grok, path)
	if err != nil || cached || first.Version != "1.0.70" || !Has(first, CapSessionID) {
		t.Fatalf("first probe = %+v, cached %v, %v", first, cached, err)
	}
	second, cached, err := pr.Probe(ctx, adapter.Grok, path)
	if err != nil || !cached || !slices.Equal(second.Capabilities, first.Capabilities) || !second.ProbedAt.Equal(first.ProbedAt) {
		t.Fatalf("second probe = %+v, cached %v, %v", second, cached, err)
	}
	modes := func() []string {
		var m []string
		for _, r := range fakecli.Records(t, f.record) {
			m = append(m, r.Mode)
		}
		return m
	}
	// The cached probe ran --version only.
	if got := strings.Join(modes(), ","); got != "version,help,version" {
		t.Errorf("runs = %s, want version,help,version", got)
	}

	// A new version is probed again, and a feature it lacks is recorded
	// as missing, not an error.
	pr.Run = f.runner(t, `{"version":"1.1.0","help":"Usage: grok\n  -p\n  --output-format <fmt>\n"}`, 10*time.Second)
	third, cached, err := pr.Probe(ctx, adapter.Grok, path)
	if err != nil || cached || third.Version != "1.1.0" {
		t.Fatalf("third probe = %+v, cached %v, %v", third, cached, err)
	}
	if !slices.Equal(third.Capabilities, []string{CapHeadless, CapJSONOutput}) {
		t.Errorf("caps after upgrade = %v", third.Capabilities)
	}
	// Another path is its own cache entry.
	other, cached, err := pr.Probe(ctx, adapter.Grok, f.fakes.Claude)
	if err != nil || cached {
		t.Errorf("other path = %+v, cached %v, %v", other, cached, err)
	}
}

func TestProbeDegrades(t *testing.T) {
	f := setup(t)
	ctx := context.Background()

	// --version fails: an error, never a panic.
	failing := func(context.Context, adapter.Command) (adapter.Output, error) {
		return adapter.Output{ExitCode: 127}, nil
	}
	pr := &Prober{Store: f.store, Run: failing}
	if _, _, err := pr.Probe(ctx, adapter.Codex, "/nowhere/codex"); err == nil {
		t.Error("probe of a failing CLI succeeded")
	}

	// codex exec --help fails: the exec capabilities are missing, the
	// rest are kept, and the result is not cached (C8).
	partial := func(_ context.Context, c adapter.Command) (adapter.Output, error) {
		switch strings.Join(c.Args, " ") {
		case "--version":
			return adapter.Output{Stdout: []byte("codex-cli 0.200.0\n")}, nil
		case "--help":
			return adapter.Output{Stdout: []byte("Commands:\n  exec   Run\n  resume  Resume\n")}, nil
		}
		return adapter.Output{ExitCode: 2, Stderr: []byte("error: unexpected argument")}, nil
	}
	pr.Run = partial
	p, cached, err := pr.Probe(ctx, adapter.Codex, "/x/codex")
	if !errors.Is(err, ErrIncomplete) || cached || !slices.Equal(p.Capabilities, []string{CapHeadless}) {
		t.Errorf("partial = %+v, %v, %v", p, cached, err)
	}
	if _, err := f.store.LatestProbe(ctx, "codex", "/x/codex", "0.200.0"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("partial probe was cached: %v", err)
	}

	// A help run that times out degrades and is not cached.
	timeout := func(_ context.Context, c adapter.Command) (adapter.Output, error) {
		if c.Args[0] == "--version" {
			return adapter.Output{Stdout: []byte("grok 2.0.0")}, nil
		}
		return adapter.Output{}, ErrTimeout
	}
	pr.Run = timeout
	p, _, err = pr.Probe(ctx, adapter.Grok, "/x/grok")
	if !errors.Is(err, ErrTimeout) || p.Version != "2.0.0" || len(p.Capabilities) != 0 {
		t.Errorf("timeout = %+v, %v", p, err)
	}
	if _, err := f.store.LatestProbe(ctx, "grok", "/x/grok", "2.0.0"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("degraded probe was cached: %v", err)
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct{ in, want string }{
		{"2.1.311 (Claude Code)\n", "2.1.311"},
		{"codex-cli 0.164.0\n", "0.164.0"},
		{"grok 1.0.70-beta.2\n", "1.0.70-beta.2"},
		{"v1.2.3\n", "1.2.3"},
	}
	for _, tt := range tests {
		if got, err := ParseVersion([]byte(tt.in), nil, nil); err != nil || got != tt.want {
			t.Errorf("ParseVersion(%q) = %q, %v, want %q", tt.in, got, err, tt.want)
		}
	}
	for _, in := range []string{"  \n", "\n  weird build xyz\n"} {
		if _, err := ParseVersion([]byte(in), nil, nil); !errors.Is(err, ErrNoVersion) {
			t.Errorf("ParseVersion(%q) = %v, want ErrNoVersion", in, err)
		}
	}
}

func TestHasFlag(t *testing.T) {
	help := "Usage: x [options]\n  -p, --print        print\n  --session-id <uuid>\n  --resume=<id>\n  [--json]\n"
	for _, f := range []string{"-p", "--print", "--session-id", "--resume", "--json"} {
		if !hasFlag(help, f) {
			t.Errorf("hasFlag(%s) = false", f)
		}
	}
	for _, f := range []string{"--session", "-i", "--fork-session", "-s"} {
		if hasFlag(help, f) {
			t.Errorf("hasFlag(%s) = true", f)
		}
	}
}
