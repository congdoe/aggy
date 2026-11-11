package hermes

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/probe"
)

const testProfile = "aggy-0123456789ab"

func TestInteractive(t *testing.T) {
	l := &adapter.HermesLaunch{Profile: testProfile}
	tests := []struct {
		name    string
		req     adapter.InteractiveRequest
		args    []string
		env     []string
		wantErr string
	}{
		{name: "new", req: adapter.InteractiveRequest{Cwd: "/repo", Hermes: l},
			args: []string{"-p", testProfile, "chat", "--cli"}},
		{name: "resume, model, brief, memory", req: adapter.InteractiveRequest{Cwd: "/repo", Hermes: l, Resume: "20261110_101500_a1b2c3",
			Model: "anthropic/claude", BriefFile: "/repo/.aggy/briefs/x.md", Memory: "remember", Extra: []string{"--provider", "x"}},
			args: []string{"-p", testProfile, "chat", "--cli", "--resume", "20261110_101500_a1b2c3", "--no-restore-cwd", "-m", "anthropic/claude", "--provider", "x", "--query-file", "/repo/.aggy/briefs/x.md"},
			env:  []string{MemoryEnv + "=remember"}},
		{name: "no profile", req: adapter.InteractiveRequest{Cwd: "/repo"}, wantErr: "no launch profile"},
		{name: "a profile aggy did not make", req: adapter.InteractiveRequest{Cwd: "/repo", Hermes: &adapter.HermesLaunch{Profile: "default"}}, wantErr: "not a profile"},
		{name: "pre-assigned id", req: adapter.InteractiveRequest{Cwd: "/repo", Hermes: l, SessionID: "x"}, wantErr: "Hermes' own"},
		{name: "fork", req: adapter.InteractiveRequest{Cwd: "/repo", Hermes: l, Fork: true}, wantErr: "forking"},
		{name: "by name", req: adapter.InteractiveRequest{Cwd: "/repo", Hermes: l, ResumeName: "x"}, wantErr: "by aggy's id"},
		{name: "bad resume id", req: adapter.InteractiveRequest{Cwd: "/repo", Hermes: l, Resume: "../x"}, wantErr: "session"},
		{name: "api key", req: adapter.InteractiveRequest{Cwd: "/repo", Hermes: l, Extra: []string{"--api-key=sk"}}, wantErr: "never passes a key"},
		{name: "base url", req: adapter.InteractiveRequest{Cwd: "/repo", Hermes: l, Extra: []string{"--base-url", "x"}}, wantErr: "never passes a key"},
		{name: "relative folder", req: adapter.InteractiveRequest{Cwd: "repo", Hermes: l}, wantErr: "working directory"},
		{name: "NUL in memory", req: adapter.InteractiveRequest{Cwd: "/repo", Hermes: l, Memory: "a\x00b"}, wantErr: "NUL"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New().Interactive(tt.req)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Path != Bin || !slices.Equal(c.Args, tt.args) || !slices.Equal(c.Env, tt.env) || c.Dir != tt.req.Cwd || c.Stdin != adapter.StdinTerminal {
				t.Fatalf("got %+v", c)
			}
		})
	}
}

func TestPassFlags(t *testing.T) {
	pf := New().PassFlags()
	find := func(name string) (adapter.RefusedFlag, bool) {
		for _, r := range pf.Refused {
			if slices.Contains(r.Names, name) {
				return r, true
			}
		}
		return adapter.RefusedFlag{}, false
	}
	tests := []struct {
		flag           string
		unlessNoPolicy bool
	}{
		{"--yolo", true}, {"--accept-hooks", true}, {"--ignore-user-config", true}, {"--safe-mode", true}, {"--ignore-rules", true},
		{"--api-key", false}, {"--base-url", false}, {"-z", false}, {"--oneshot", false}, {"-p", false}, {"--profile", false},
		{"--tui", false}, {"--query-file", false}, {"-q", false}, {"--no-restore-cwd", false}, {"--worktree", false},
	}
	for _, tt := range tests {
		r, ok := find(tt.flag)
		if !ok || r.UnlessNoPolicy != tt.unlessNoPolicy {
			t.Errorf("%s: refused %v, unless --no-policy %v; want refused, %v", tt.flag, ok, r.UnlessNoPolicy, tt.unlessNoPolicy)
		}
	}
	if !slices.Contains(pf.Commands, "chat") {
		t.Error("chat is not a subcommand, so a prompt word chat would reach Hermes")
	}
}

// TestLoosenEffect (T6979): every flag aggy lets through only with
// --no-policy, or a yes asked in place, says what it does on Hermes.
func TestLoosenEffect(t *testing.T) {
	pf := New().PassFlags()
	for _, r := range pf.Refused {
		if !r.UnlessNoPolicy {
			continue
		}
		for _, f := range r.Names {
			if e := loosenEffect(f); e == "" || !strings.Contains(pf.LoosenEffect(f), e) {
				t.Errorf("%s: no effect of its own (%q)", f, pf.LoosenEffect(f))
			}
		}
	}
}

func TestReachable(t *testing.T) {
	tests := []struct {
		caps []string
		want string
	}{
		{[]string{probe.CapClassicScreen, probe.CapResume}, ""},
		{[]string{probe.CapResume}, "--cli"},
		{[]string{probe.CapClassicScreen}, "--resume"},
	}
	for _, tt := range tests {
		got := Reachable(tt.caps)
		if (tt.want == "") != (got == "") || !strings.Contains(got, tt.want) {
			t.Errorf("Reachable(%v) = %q, want %q", tt.caps, got, tt.want)
		}
	}
}

func TestProfileName(t *testing.T) {
	a, b := ProfileName("/repo/a"), ProfileName("/repo/b")
	if a == b || !ValidProfile(a) || !ValidProfile(b) || a != ProfileName("/repo/a") {
		t.Fatalf("ProfileName: %q %q", a, b)
	}
	for _, p := range []string{"default", "aggy-", "aggy-0123456789abc", "aggy-0123456789AB", "x-0123456789ab", ""} {
		if ValidProfile(p) {
			t.Errorf("ValidProfile(%q) = true", p)
		}
	}
}

func TestCommands(t *testing.T) {
	tests := []struct {
		name    string
		build   func() (adapter.Command, error)
		args    []string
		wantErr bool
	}{
		{"create", func() (adapter.Command, error) { return ProfileCreateCommand("/bin/hermes", testProfile) }, []string{"profile", "create", testProfile, "--no-alias", "--no-skills"}, false},
		{"create default", func() (adapter.Command, error) { return ProfileCreateCommand("/bin/hermes", "default") }, nil, true},
		{"get root", func() (adapter.Command, error) { return ConfigGetCommand("/bin/hermes", "", AdoptExternalLogins) }, []string{"config", "get", AdoptExternalLogins, "--json"}, false},
		{"get profile", func() (adapter.Command, error) { return ConfigGetCommand("/bin/hermes", testProfile, "approvals.mode") }, []string{"-p", testProfile, "config", "get", "approvals.mode", "--json"}, false},
		{"get bad key", func() (adapter.Command, error) { return ConfigGetCommand("/bin/hermes", "", "a;b") }, nil, true},
		{"export", func() (adapter.Command, error) {
			return ExportCommand("/bin/hermes", testProfile, "20261110_101500_a1b2c3")
		},
			[]string{"-p", testProfile, "sessions", "export", "-", "--session-id", "20261110_101500_a1b2c3", "--format", "jsonl", "--redact"}, false},
		{"export bad id", func() (adapter.Command, error) { return ExportCommand("/bin/hermes", testProfile, "--all") }, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := tt.build()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("got %v, want an error", c.Args)
				}
				return
			}
			if err != nil || c.Path != "/bin/hermes" || !slices.Equal(c.Args, tt.args) || c.Stdin != adapter.StdinNull || len(c.Env) != 0 {
				t.Fatalf("got %+v, %v", c, err)
			}
		})
	}
}

func TestParseBool(t *testing.T) {
	tests := []struct {
		in    string
		v, ok bool
	}{
		{"true\n", true, true}, {"false", false, true}, {`"true"`, false, false}, {"", false, false}, {"null", false, true},
	}
	for _, tt := range tests {
		v, ok := ParseBool([]byte(tt.in))
		if v != tt.v || ok != tt.ok {
			t.Errorf("ParseBool(%q) = %v, %v", tt.in, v, ok)
		}
	}
}

func TestParseExport(t *testing.T) {
	// Hand-built from the writer's source (testdata/README.md).
	b, err := os.ReadFile(filepath.Join("testdata", "export.handbuilt.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := ParseExport(b, 0)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range tr.Turns {
		got = append(got, x.Role+":"+x.Text)
	}
	want := []string{"user:fix the build", "assistant:The build passes now.", "user:thanks"}
	if !slices.Equal(got, want) {
		t.Fatalf("turns = %q, want %q", got, want)
	}
	if tr.Turns[1].Time.UnixMilli() != 1794305704250 {
		t.Errorf("time = %v", tr.Turns[1].Time)
	}
	tr, _ = ParseExport(b, 2)
	if len(tr.Turns) != 2 || tr.Turns[0].Text != "The build passes now." {
		t.Errorf("last two = %+v", tr.Turns)
	}
	for _, bad := range []string{"", "not json", `{"messages":[]}`, `{"id":"x"}`, `[1]`} {
		if _, err := ParseExport([]byte(bad), 0); !errors.Is(err, adapter.ErrNoTranscript) {
			t.Errorf("ParseExport(%q) = %v, want ErrNoTranscript", bad, err)
		}
	}
	if bytes.Count(b, []byte("\n")) != 1 {
		t.Error("the fixture is one line")
	}
}

func TestHookCommand(t *testing.T) {
	got, err := HookCommand("/usr/local/bin/aggy", "/home/u/.aggy", testProfile, EventPreToolCall)
	if err != nil || got != "/usr/local/bin/aggy hook hermes --home /home/u/.aggy --profile "+testProfile+" --event pre_tool_call" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, c := range []struct{ bin, home, profile, event string }{
		{"aggy", "/h", testProfile, EventPreToolCall},
		{"/a b/aggy", "/h", testProfile, EventPreToolCall},
		{"/aggy", "/h'x", testProfile, EventPreToolCall},
		{"/aggy", "/h", "default", EventPreToolCall},
		{"/aggy", "/h", testProfile, "post_llm_call"},
	} {
		if _, err := HookCommand(c.bin, c.home, c.profile, c.event); err == nil {
			t.Errorf("HookCommand(%+v) took it", c)
		}
	}
}

func TestBlock(t *testing.T) {
	b := Block{Bin: "/bin/aggy", Home: "/h/.aggy", Profile: testProfile, Deny: []string{"*rm -rf /*", "*git push --force*"}}
	block, err := b.Render()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{BlockStart + "\n", "  mode: manual\n", `    - "*rm -rf /*"` + "\n", "      fail_closed: true\n",
		"hooks_auto_accept: false\n", "  check: false\n", "  adopt_external_logins: false\n", BlockEnd + "\n",
		`    - command: "/bin/aggy hook hermes --home /h/.aggy --profile ` + testProfile + ` --event on_session_end"`} {
		if !strings.Contains(string(block), want) {
			t.Errorf("block lacks %q:\n%s", want, block)
		}
	}
	if _, err := (Block{Bin: "/bin/aggy", Home: "/h", Profile: testProfile, Deny: []string{`a"b`}}).Render(); err == nil {
		t.Error("a quote in a deny glob was taken")
	}
	empty, _ := Block{Bin: "/bin/aggy", Home: "/h", Profile: testProfile}.Render()
	if !strings.Contains(string(empty), "  deny: []\n") {
		t.Errorf("no deny list:\n%s", empty)
	}

	tests := []struct {
		name    string
		src     string
		want    string
		wantErr bool
	}{
		{"empty", "", string(block), false},
		{"seeded", "model:\n  default: x", "model:\n  default: x\n" + string(block), false},
		{"replaced", "model: x\n" + string(empty) + "other: 1\n", "model: x\nother: 1\n" + string(block), false},
		{"owned key outside", "approvals:\n  mode: off\n", "", true},
		{"owned key in a list is fine", "x:\n  - hooks: 1\n", "x:\n  - hooks: 1\n" + string(block), false},
		{"nested owned key is fine", "x:\n  auth: 1\n", "x:\n  auth: 1\n" + string(block), false},
		{"start without end", BlockStart + "\nx: 1\n", "", true},
		{"two starts", BlockStart + "\n" + BlockStart + "\n" + BlockEnd + "\n", "", true},
		{"end first", BlockEnd + "\n" + BlockStart + "\n", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SetBlock([]byte(tt.src), block)
			if tt.wantErr {
				if !errors.Is(err, ErrBlock) {
					t.Fatalf("err = %v, want ErrBlock", err)
				}
				return
			}
			if err != nil || string(got) != tt.want {
				t.Fatalf("got %q, %v\nwant %q", got, err, tt.want)
			}
			again, err := SetBlock(got, block)
			if err != nil || !bytes.Equal(again, got) {
				t.Fatalf("not idempotent: %q", again)
			}
			back, err := StripBlock(got)
			if err != nil || strings.Contains(string(back), BlockStart) {
				t.Fatalf("strip: %q, %v", back, err)
			}
		})
	}
}
