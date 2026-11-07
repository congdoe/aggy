package opencode

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
)

const testID = "ses_0123456789abcdef"

func TestInteractive(t *testing.T) {
	launch := func(mini, auto bool) *adapter.OpenCodeLaunch {
		return &adapter.OpenCodeLaunch{Mini: mini, Auto: auto, ConfigFile: "/run/l/opencode.json", ConfigDir: "/run/l/config"}
	}
	cases := []struct {
		name    string
		req     adapter.InteractiveRequest
		want    []string
		wantErr string
	}{
		{"full screen, new", adapter.InteractiveRequest{Cwd: "/r", SessionID: testID, OpenCode: launch(false, false)},
			[]string{"--standalone", "--session", testID}, ""},
		{"full screen, resume, model stays in the config", adapter.InteractiveRequest{Cwd: "/r", Resume: testID, Model: "xai/grok-build-0.1", OpenCode: launch(false, false)},
			[]string{"--standalone", "--session", testID}, ""},
		{"full auto", adapter.InteractiveRequest{Cwd: "/r", SessionID: testID, OpenCode: launch(false, true)},
			[]string{"--standalone", "--session", testID, "--auto"}, ""},
		{"mini takes -m", adapter.InteractiveRequest{Cwd: "/r", SessionID: testID, Model: "xai/grok-build-0.1", OpenCode: launch(true, false)},
			[]string{"mini", "--standalone", "--session", testID, "-m", "xai/grok-build-0.1"}, ""},
		{"mini has no auto", adapter.InteractiveRequest{Cwd: "/r", SessionID: testID, OpenCode: launch(true, true)}, nil, "no --auto"},
		{"no launch fails closed", adapter.InteractiveRequest{Cwd: "/r", SessionID: testID}, nil, "rules would not reach"},
		{"relative config", adapter.InteractiveRequest{Cwd: "/r", SessionID: testID, OpenCode: &adapter.OpenCodeLaunch{ConfigFile: "x", ConfigDir: "/y"}}, nil, "config file"},
		{"a uuid is not an opencode id", adapter.InteractiveRequest{Cwd: "/r", SessionID: "4f1c2d3e-0000-4000-8000-000000000000", OpenCode: launch(false, false)}, nil, "not ses_"},
		{"an id that would be a flag", adapter.InteractiveRequest{Cwd: "/r", Resume: "--auto", OpenCode: launch(false, false)}, nil, "starts with -"},
		{"a model that would be a flag", adapter.InteractiveRequest{Cwd: "/r", SessionID: testID, Model: "--auto", OpenCode: launch(true, false)}, nil, "not a model name"},
		{"fork", adapter.InteractiveRequest{Cwd: "/r", Resume: testID, Fork: true, OpenCode: launch(false, false)}, nil, "not supported"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd, err := New().Interactive(c.req)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cmd.Args, c.want) {
				t.Fatalf("args = %q, want %q", cmd.Args, c.want)
			}
			if cmd.Path != Bin || cmd.Dir != "/r" || cmd.Stdin != adapter.StdinTerminal {
				t.Fatalf("cmd = %+v", cmd)
			}
		})
	}
}

// TestInteractiveEnv holds the launch's own variables: the design's, the
// location, and the git config, each named exactly.
func TestInteractiveEnv(t *testing.T) {
	cmd, err := New().Interactive(adapter.InteractiveRequest{Cwd: "/r", SessionID: testID, Env: []string{"AGGY_VENDOR=opencode"},
		OpenCode: &adapter.OpenCodeLaunch{ConfigFile: "/l/c.json", ConfigDir: "/l/d"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"AGGY_VENDOR=opencode", "OPENCODE_DISABLE_PROJECT_CONFIG=1", "OPENCODE_CONFIG=/l/c.json", "OPENCODE_CONFIG_DIR=/l/d",
		"OPENCODE_DISABLE_AUTOUPDATE=1", "PWD=/r", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=false"}
	if !reflect.DeepEqual(cmd.Env, want) {
		t.Fatalf("env = %q, want %q", cmd.Env, want)
	}
}

func TestHeadlessRefuses(t *testing.T) {
	if _, err := New().Headless(adapter.HeadlessRequest{Kind: adapter.Review}); !errors.Is(err, adapter.ErrUnsupported) {
		t.Fatalf("Headless err = %v, want ErrUnsupported, so a borrow passes it over", err)
	}
}

func TestReachable(t *testing.T) {
	all := []string{probe.CapStandalone, probe.CapSession, probe.CapMiniSession}
	cases := []struct {
		name string
		caps []string
		mini bool
		want string
	}{
		{"full screen", all, false, ""},
		{"mini", all, true, ""},
		{"no probe", nil, false, "--standalone"},
		{"no standalone", []string{probe.CapSession}, false, "--standalone"},
		{"full screen, no session", []string{probe.CapStandalone, probe.CapMiniSession}, false, "--session"},
		{"mini, no session", []string{probe.CapStandalone, probe.CapSession}, true, "mini --help"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Reachable(c.caps, c.mini)
			if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestProbe(t *testing.T) {
	run := func(_ context.Context, c adapter.Command) (adapter.Output, error) {
		switch strings.Join(c.Args, " ") {
		case "--version":
			return adapter.Output{Stdout: []byte("2.2.5\n")}, nil
		case "--help":
			return adapter.Output{Stdout: []byte("  --standalone  private\n  --session, -s string\n  --auto\n")}, nil
		case "mini --help":
			return adapter.Output{Stdout: []byte("  --session, -s string\n  --model, -m string\n")}, nil
		}
		t.Fatalf("unexpected args %q", c.Args)
		return adapter.Output{}, nil
	}
	p, err := Adapter{Path: "/bin/opencode"}.Probe(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if p.Version != "2.2.5" {
		t.Fatalf("version = %q", p.Version)
	}
	for _, c := range []string{probe.CapStandalone, probe.CapSession, probe.CapAuto, probe.CapMiniSession} {
		if !slices.Contains(p.Capabilities, c) {
			t.Fatalf("caps %q lack %s", p.Capabilities, c)
		}
	}
	if w := adapter.VersionWarning(Adapter{}, p.Version, ""); w != "" {
		t.Fatalf("tested version warns: %s", w)
	}
	if w := adapter.VersionWarning(Adapter{}, "2.2.6", ""); w == "" {
		t.Fatal("a newer version should warn: aggy tests a pinned one")
	}
}

func TestExportCommand(t *testing.T) {
	cmd, err := ExportCommand("/bin/opencode", testID, "/repo", "/run/empty")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"session", "export", testID, "--standalone"}; !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("args = %q", cmd.Args)
	}
	if !slices.Contains(cmd.Env, "OPENCODE_DISABLE_PROJECT_CONFIG=1") || !slices.Contains(cmd.Env, "OPENCODE_CONFIG_DIR=/run/empty") {
		t.Fatalf("env = %q: the repo's and the global config must stay off", cmd.Env)
	}
	if _, err := ExportCommand("/bin/opencode", "../x", "/repo", "/run/empty"); err == nil {
		t.Fatal("a bad id reached argv")
	}
}

func TestParseExport(t *testing.T) {
	good, err := safefs.ReadFile("testdata/export.json")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		in      string
		n       int
		want    []string
		wantErr bool
	}{
		{"every turn, no reasoning or tool or idle", string(good), 0,
			[]string{"user: Remember this made-up word: quillomet.", "assistant: OK", "user: What was the word?", "assistant: quillomet"}, false},
		{"the latest two", string(good), 2, []string{"user: What was the word?", "assistant: quillomet"}, false},
		{"no messages key", `{"info":{}}`, 0, nil, true},
		{"no info key", `{"messages":[]}`, 0, nil, true},
		{"not json", `error: session not found`, 0, nil, true},
		{"cut off by the output cap", string(good[:len(good)/2]), 0, nil, true},
		{"empty session", `{"info":{},"messages":[]}`, 0, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tr, err := ParseExport([]byte(c.in), c.n)
			if c.wantErr {
				if !errors.Is(err, adapter.ErrNoTranscript) {
					t.Fatalf("err = %v, want ErrNoTranscript", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, tu := range tr.Turns {
				got = append(got, tu.Role+": "+tu.Text)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestValidSessionID(t *testing.T) {
	for _, c := range []struct {
		id string
		ok bool
	}{
		{"ses_aggy000A", true}, {NewSessionID("4f1c2d3e-0000-4000-8000-000000000000"), true},
		{"ses", false}, {"ses_", false}, {"ses_a/b", false}, {"ses_a b", false}, {"-ses_a", false},
		{"4f1c2d3e-0000-4000-8000-000000000000", false}, {"ses_" + strings.Repeat("a", 65), false}, {"", false},
	} {
		if err := adapter.ValidSessionID(adapter.OpenCode, c.id); (err == nil) != c.ok {
			t.Errorf("%q: err = %v, want ok=%v", c.id, err, c.ok)
		}
	}
}
