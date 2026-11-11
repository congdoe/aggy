package antigravity

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/claude"
	"github.com/congdoe/aggy/internal/adapter/codex"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/adapter/hermes"
	"github.com/congdoe/aggy/internal/adapter/opencode"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
)

// TestPassFlags: every flag in its single- and double-dash spelling, with =
// values and =true/=false, meets the same check (T6957).
func TestPassFlags(t *testing.T) {
	pf := New().PassFlags()
	tests := []struct {
		name    string
		args    []string
		want    error  // errors.Is, or nil
		refuses string // a substring of a refusal that is not a sentinel
		check   func(t *testing.T, p adapter.ParsedFlags)
	}{
		{name: "model", args: []string{"--model", "gemini-3.8-flash-high"}, check: func(t *testing.T, p adapter.ParsedFlags) {
			if p.Model != "gemini-3.8-flash-high" || len(p.Extra) != 0 {
				t.Errorf("model %q extra %v", p.Model, p.Extra)
			}
		}},
		{name: "model =", args: []string{"--model=m1"}, check: func(t *testing.T, p adapter.ParsedFlags) {
			if p.Model != "m1" {
				t.Errorf("model %q", p.Model)
			}
		}},
		// #141 review, M2: only the canonical spelling is read as the flag.
		{name: "model one dash refused", args: []string{"-model=m1"}, want: adapter.ErrNotChecked},
		{name: "prompt-interactive one dash refused", args: []string{"-prompt-interactive", "x"}, want: adapter.ErrNotChecked},
		{name: "print one dash refused", args: []string{"-print"}, want: adapter.ErrNotChecked},
		{name: "continue", args: []string{"-c"}, check: func(t *testing.T, p adapter.ParsedFlags) {
			if !p.Continue {
				t.Error("not continue")
			}
		}},
		{name: "continue two dashes one letter refused", args: []string{"--c"}, want: adapter.ErrNotChecked},
		{name: "continue long one dash refused", args: []string{"-continue"}, want: adapter.ErrNotChecked},
		{name: "continue long", args: []string{"--continue"}, check: func(t *testing.T, p adapter.ParsedFlags) {
			if !p.Continue {
				t.Error("not continue")
			}
		}},
		{name: "continue =false", args: []string{"-c=false"}, refuses: "with no value"},
		{name: "conversation", args: []string{"--conversation=11111111-2222-4333-8444-555555555555"}, check: func(t *testing.T, p adapter.ParsedFlags) {
			if !p.Resume || p.ResumeID != "11111111-2222-4333-8444-555555555555" {
				t.Errorf("resume %v %q", p.Resume, p.ResumeID)
			}
		}},
		{name: "effort", args: []string{"--effort", "high"}, check: func(t *testing.T, p adapter.ParsedFlags) {
			if !slices.Equal(p.Extra, []string{"--effort", "high"}) || len(p.Prompt) != 0 {
				t.Errorf("extra %v prompt %v", p.Extra, p.Prompt)
			}
		}},
		{name: "sandbox noted", args: []string{"--sandbox"}, check: func(t *testing.T, p adapter.ParsedFlags) {
			if len(p.Notes) != 1 {
				t.Errorf("notes %v", p.Notes)
			}
		}},
		{name: "sandbox =true", args: []string{"--sandbox=true"}, check: func(t *testing.T, p adapter.ParsedFlags) {
			if len(p.Notes) != 1 {
				t.Errorf("notes %v", p.Notes)
			}
		}},
		// #141 review, L2: the note says the sandbox is on, so off is refused.
		{name: "sandbox =false refused", args: []string{"--sandbox=false"}, refuses: "leave it out"},
		{name: "sandbox =0 refused", args: []string{"--sandbox=0"}, refuses: "leave it out"},
		{name: "first prompt -i", args: []string{"-i", "fix it"}, check: func(t *testing.T, p adapter.ParsedFlags) {
			if len(p.Prompt) != 0 || !slices.Contains(p.Once, "-i") || len(p.Safe) != 0 {
				t.Errorf("prompt %v once %v safe %v", p.Prompt, p.Once, p.Safe)
			}
		}},
		{name: "first prompt -i=", args: []string{"-i=fix"}, check: func(t *testing.T, p adapter.ParsedFlags) {
			if !slices.Contains(p.Once, "-i") {
				t.Errorf("once %v", p.Once)
			}
		}},
		{name: "first prompt --i refused", args: []string{"--i=fix"}, want: adapter.ErrNotChecked},
		{name: "mode plan", args: []string{"--mode", "plan"}},
		{name: "mode plan =", args: []string{"--mode=plan"}},
		{name: "mode auto", args: []string{"--mode", "auto"}, want: adapter.ErrLoosens},
		{name: "mode auto =", args: []string{"--mode=auto"}, want: adapter.ErrLoosens},
		{name: "mode auto one dash refused", args: []string{"-mode=auto"}, want: adapter.ErrNotChecked},
		{name: "mode accept-edits", args: []string{"--mode", "accept-edits"}, want: adapter.ErrLoosens},
		{name: "mode unknown", args: []string{"--mode=yolo"}, want: adapter.ErrLoosens},
		{name: "skip permissions", args: []string{"--dangerously-skip-permissions"}, want: adapter.ErrLoosens},
		{name: "skip permissions one dash refused", args: []string{"-dangerously-skip-permissions"}, want: adapter.ErrNotChecked},
		{name: "skip permissions =true", args: []string{"--dangerously-skip-permissions=true"}, want: adapter.ErrLoosens},
		{name: "skip permissions =false", args: []string{"--dangerously-skip-permissions=false"}, want: adapter.ErrLoosens},
		{name: "add-dir", args: []string{"--add-dir", "../other"}, want: adapter.ErrLoosens},
		{name: "add-dir =", args: []string{"--add-dir=../other"}, want: adapter.ErrLoosens},
		{name: "print", args: []string{"-p"}, refuses: "on screen only"},
		{name: "print long", args: []string{"--print"}, refuses: "on screen only"},

		{name: "print =true", args: []string{"-p=true"}, refuses: "on screen only"},
		{name: "prompt alias", args: []string{"--prompt", "x"}, refuses: "on screen only"},
		{name: "output format", args: []string{"--output-format=json"}, refuses: "on screen only"},
		{name: "input format", args: []string{"--input-format", "text"}, refuses: "on screen only"},
		{name: "json schema", args: []string{"--json-schema={}"}, refuses: "on screen only"},
		{name: "print timeout", args: []string{"--print-timeout=5"}, refuses: "on screen only"},
		{name: "settings", args: []string{"--settings", "{}"}, refuses: "own --settings"},
		{name: "settings =", args: []string{"--settings={}"}, refuses: "own --settings"},
		{name: "remote control", args: []string{"--remote-control"}, refuses: "does not supervise"},
		{name: "remote agent", args: []string{"--remote-agent=x"}, refuses: "does not supervise"},
		{name: "project", args: []string{"--project", "x"}, refuses: "changes what aggy starts"},
		{name: "new project", args: []string{"--new-project=x"}, refuses: "changes what aggy starts"},
		{name: "agent", args: []string{"--agent", "x"}, refuses: "changes what aggy starts"},
		{name: "log file", args: []string{"--log-file=/tmp/x"}, refuses: "log"},
		{name: "slash commands", args: []string{"--disable-slash-commands"}, refuses: "/exit"},
		{name: "unknown flag", args: []string{"--no-such-flag"}, want: adapter.ErrNotChecked},
		{name: "unknown flag one dash", args: []string{"-yolo"}, want: adapter.ErrNotChecked},
		{name: "three dashes", args: []string{"---model=x"}, want: adapter.ErrNotChecked},
		{name: "subcommand word", args: []string{"models"}, want: adapter.ErrNotFlag},
		{name: "subcommand after flag", args: []string{"--effort", "high", "update"}, want: adapter.ErrNotFlag},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := pf.Parse(tt.args, false)
			switch {
			case tt.want != nil:
				if !errors.Is(err, tt.want) {
					t.Fatalf("Parse(%v) = %v, want %v", tt.args, err, tt.want)
				}
			case tt.refuses != "":
				if err == nil || !strings.Contains(err.Error(), tt.refuses) {
					t.Fatalf("Parse(%v) = %v, want a refusal with %q", tt.args, err, tt.refuses)
				}
			case err != nil:
				t.Fatalf("Parse(%v) = %v", tt.args, err)
			}
			if tt.check != nil {
				tt.check(t, p)
			}
		})
	}
}

// TestLoosenAsked: a loosening flag the person let through passes, in either
// spelling, and is named as the table names it.
func TestLoosenAsked(t *testing.T) {
	pf := New().PassFlags()
	for _, tt := range []struct {
		args    []string
		allowed string
	}{
		{[]string{"--dangerously-skip-permissions"}, "--dangerously-skip-permissions"},
		{[]string{"--mode=auto"}, "--mode auto"},
		{[]string{"--add-dir", "../x"}, "--add-dir"},
	} {
		_, err := pf.Parse(tt.args, false)
		if f, ok := adapter.LoosenedFlag(err); !ok || f != tt.allowed {
			t.Errorf("%v: loosened %q %v, want %q", tt.args, f, ok, tt.allowed)
		}
		p, err := pf.ParseWith(tt.args, adapter.ParseOpts{Allowed: []string{tt.allowed}})
		if err != nil || len(p.Loosens) != 1 {
			t.Errorf("%v allowed: %v %v", tt.args, p.Loosens, err)
		}
	}
	if pf.LoosenEffect("--dangerously-skip-permissions") == pf.LoosenEffect("--unknown") {
		t.Error("no effect line for --dangerously-skip-permissions")
	}
}

// TestModes: print flags in either spelling run print mode (refused at
// launch), and --version or -version alone runs bare.
func TestModes(t *testing.T) {
	pf := New().PassFlags()
	for _, a := range [][]string{{"-p"}, {"--print"}, {"--print=true"}, {"-p=true"}, {"--prompt", "x"}, {"--output-format=json"}} {
		if pf.Mode(a) != adapter.RunPrint {
			t.Errorf("Mode(%v) = %v, want print", a, pf.Mode(a))
		}
	}
	for _, a := range [][]string{{"--version"}, {"-h"}, {"--help"}} {
		if pf.Mode(a) != adapter.RunBare {
			t.Errorf("Mode(%v) = %v, want bare", a, pf.Mode(a))
		}
	}
	if pf.Mode([]string{"-version"}) == adapter.RunBare {
		t.Error("-version, not the canonical spelling, ran bare")
	}
	if pf.Mode([]string{"--version", "-c"}) == adapter.RunBare {
		t.Error("a version flag with another word ran bare")
	}
}

// TestOtherTablesUnchanged: GoFlags is Antigravity's alone, so a single-dash
// long flag on the others is still read their way.
func TestOtherTablesUnchanged(t *testing.T) {
	for _, a := range []adapter.Adapter{claude.New(), codex.New(), grok.New(), opencode.New(), hermes.New()} {
		if a.PassFlags().GoFlags {
			t.Errorf("%s has GoFlags", a.Vendor())
		}
	}
	// Claude's -p with a value attached stays print mode, its cluster read.
	if claude.New().PassFlags().Mode([]string{"-pfoo"}) != adapter.RunPrint {
		t.Error("claude -pfoo is no longer print mode")
	}
	// Hermes' abbreviation refusal keeps its own error.
	if _, err := hermes.New().PassFlags().Parse([]string{"--yol"}, false); !errors.Is(err, adapter.ErrNotListed) {
		t.Errorf("hermes --yol = %v", err)
	}
}

func launch(t *testing.T) *adapter.AntigravityLaunch {
	t.Helper()
	return &adapter.AntigravityLaunch{SettingsFile: "/run/x/antigravity-settings.json", Plan: "/run/x/policy-decide.json", PlanSum: strings.Repeat("a", 64)}
}

// TestInteractive: the argv and environment each launch builds.
func TestInteractive(t *testing.T) {
	const id = "11111111-2222-4333-8444-555555555555"
	tests := []struct {
		name    string
		req     adapter.InteractiveRequest
		args    []string
		wantErr bool
	}{
		{"new", adapter.InteractiveRequest{Cwd: "/r"}, []string{"--settings", "/run/x/antigravity-settings.json"}, false},
		{"resume", adapter.InteractiveRequest{Cwd: "/r", Resume: id, Model: "m"},
			[]string{"--settings", "/run/x/antigravity-settings.json", "--conversation=" + id, "--model", "m"}, false},
		{"brief", adapter.InteractiveRequest{Cwd: "/r", BriefFile: "/r/.aggy/briefs/c.md", Extra: []string{"--effort", "high"}},
			[]string{"--settings", "/run/x/antigravity-settings.json", "--effort", "high", "-i", adapter.BriefPrompt("/r/.aggy/briefs/c.md")}, false},
		{"session id", adapter.InteractiveRequest{Cwd: "/r", SessionID: id}, nil, true},
		{"print", adapter.InteractiveRequest{Cwd: "/r", Print: true}, nil, true},
		{"fork", adapter.InteractiveRequest{Cwd: "/r", Resume: id, Fork: true}, nil, true},
		{"by name", adapter.InteractiveRequest{Cwd: "/r", ResumeName: "x"}, nil, true},
		{"bad resume", adapter.InteractiveRequest{Cwd: "/r", Resume: "-c"}, nil, true},
		{"own settings", adapter.InteractiveRequest{Cwd: "/r", Extra: []string{"-settings={}"}}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.req.Antigravity = launch(t)
			c, err := New().Interactive(tt.req)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("built %v", c.Args)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Path != Bin || c.Dir != "/r" || c.Stdin != adapter.StdinTerminal || !slices.Equal(c.Args, tt.args) {
				t.Errorf("command %+v, want args %v", c, tt.args)
			}
			if !slices.Contains(c.Env, PlanEnv+"=/run/x/policy-decide.json") || !slices.Contains(c.Env, PlanSumEnv+"="+strings.Repeat("a", 64)) {
				t.Errorf("env %v", c.Env)
			}
		})
	}
	if _, err := New().Interactive(adapter.InteractiveRequest{Cwd: "/r"}); err == nil {
		t.Error("built a command with no launch settings")
	}
	c, err := New().Interactive(adapter.InteractiveRequest{Cwd: "/r", Antigravity: &adapter.AntigravityLaunch{SettingsFile: "/run/x/s.json"}})
	if err != nil || slices.ContainsFunc(c.Env, func(kv string) bool { return strings.HasPrefix(kv, "AGGY_DECIDE") }) {
		t.Errorf("--no-policy launch: %v %v", c.Env, err)
	}
	if _, err := New().Headless(adapter.HeadlessRequest{}); !errors.Is(err, adapter.ErrUnsupported) {
		t.Errorf("Headless = %v", err)
	}
	if _, err := New().TranscriptTail(adapter.SessionRef{}, 5); !errors.Is(err, adapter.ErrNoTranscript) {
		t.Errorf("TranscriptTail = %v", err)
	}
}

// TestSettings: the policy as agy rules, asks and denies only, no allow.
func TestSettings(t *testing.T) {
	ds := (parity.PolicyPlan{OnScreen: true, Settings: map[string]permissions.Kind{permissions.PolicyRmRf: permissions.Deny}}).Decide()
	var rs []Rule
	for _, d := range ds {
		rs = append(rs, Rule{d.Text, d.Kind})
	}
	b, err := Settings(rs)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Permissions map[string][]string `json:"permissions"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Permissions["allow"]; ok || len(s.Permissions) != 2 {
		t.Fatalf("permissions %v", s.Permissions)
	}
	if !slices.Contains(s.Permissions["ask"], "read_file(**/.env)") || !slices.Contains(s.Permissions["ask"], "command(git push * main)") {
		t.Errorf("ask %v", s.Permissions["ask"])
	}
	if !slices.Contains(s.Permissions["deny"], "command(rm -rf*)") || slices.Contains(s.Permissions["ask"], "command(rm -rf*)") {
		t.Errorf("deny %v", s.Permissions["deny"])
	}
	empty, _ := Settings(nil)
	if string(empty) != `{"permissions":{}}` {
		t.Errorf("no decisions: %s", empty)
	}
}

// TestHooksFile: aggy's entry merges into a file without touching other
// entries, comes out again byte for byte, and a file aggy cannot parse is
// refused.
func TestHooksFile(t *testing.T) {
	entry, err := Entry("/usr/local/bin/aggy")
	if err != nil {
		t.Fatal(err)
	}
	var e map[string][]map[string]any
	if err := json.Unmarshal(entry, &e); err != nil || len(e["PreToolUse"]) != 1 || e["PreToolUse"][0]["matcher"] != ".*" || len(e["Stop"]) != 1 || len(e["SessionStart"]) != 1 {
		t.Fatalf("entry %s: %v", entry, err)
	}
	if !strings.Contains(string(entry), "/usr/local/bin/aggy hook antigravity --event PreToolUse") {
		t.Errorf("entry %s", entry)
	}
	theirs := "{\n  \"mine\": {\"Stop\": [{\"type\":\"command\",\"command\":\"echo hi\",\"timeout\":5}]}\n}\n"
	for _, src := range []string{"", theirs, "{}", "{\n}\n", `{"aggy": {"Stop": []}, "mine": {}}`, `{"mine": {}, "aggy": 1, "z": 2}`} {
		var in []byte
		if src != "" {
			in = []byte(src)
		}
		out, err := SetHooks(in, entry)
		if err != nil {
			t.Fatalf("SetHooks(%q): %v", src, err)
		}
		if !json.Valid(out) || !HasEntry(out, entry) {
			t.Fatalf("SetHooks(%q) = %s", src, out)
		}
		again, err := SetHooks(out, entry)
		if err != nil || string(again) != string(out) {
			t.Errorf("not idempotent on %q: %s", src, again)
		}
		back, err := StripHooks(out)
		if err != nil || !json.Valid(back) || HasEntry(back, entry) {
			t.Fatalf("StripHooks(%s) = %s, %v", out, back, err)
		}
		if src == theirs && string(back) != theirs {
			t.Errorf("their file changed:\n%s\nwant\n%s", back, theirs)
		}
		if strings.Contains(src, "mine") && !strings.Contains(string(back), "mine") {
			t.Errorf("their entry is gone: %s", back)
		}
	}
	// #141 review, L6: every byte outside aggy's entry comes back, the space
	// inside empty braces included.
	for _, src := range []string{"{\n\n   \n}\n", "{}", "{ }", "\n{\t}\n\n", theirs, "{\"a\": 1 ,\n\n \"b\" : [ ]\n}"} {
		out, err := SetHooks([]byte(src), entry)
		if err != nil {
			t.Fatal(err)
		}
		if back, err := StripHooks(out); err != nil || string(back) != src {
			t.Errorf("round trip of %q gave %q, %v", src, back, err)
		}
	}
	if f, err := ForeignEntry([]byte(theirs)); err != nil || f {
		t.Errorf("no aggy entry: foreign %v %v", f, err)
	}
	if mine, _ := SetHooks([]byte(theirs), entry); func() bool { f, _ := ForeignEntry(mine); return f }() {
		t.Error("aggy's own entry read as foreign")
	}
	other, _ := Entry("/opt/aggy/bin/aggy")
	if moved, _ := SetHooks([]byte(theirs), other); func() bool { f, _ := ForeignEntry(moved); return f }() {
		t.Error("aggy's entry from another aggy path read as foreign")
	}
	for _, f := range []string{`{"aggy": {"Stop": []}}`, `{"aggy": 1}`, `{"aggy": {"SessionStart": [{"type":"command","command":"/x/aggy hook antigravity --event SessionStart","timeout":5}]}}`} {
		if foreign, _ := ForeignEntry([]byte(f)); !foreign {
			t.Errorf("%s not foreign", f)
		}
	}
	for _, bad := range []string{"[]", "not json", `{"a":1} x`, `{"aggy":{},"aggy":{}}`, `{"a":`} {
		if _, err := SetHooks([]byte(bad), entry); !errors.Is(err, ErrHooksFile) {
			t.Errorf("SetHooks(%q) = %v, want ErrHooksFile", bad, err)
		}
	}
	if _, err := HookCommand("relative/aggy", "Stop"); err == nil {
		t.Error("a relative aggy path went into a hook command")
	}
	if _, err := HookCommand("/bin/aggy", "PostToolUse"); err == nil {
		t.Error("an event aggy does not hook went in")
	}
}

// TestReachable: --settings and --conversation are needed.
func TestReachable(t *testing.T) {
	if Reachable([]string{"settings", "resume"}) != "" || Reachable([]string{"resume"}) == "" || Reachable([]string{"settings"}) == "" {
		t.Error("Reachable")
	}
}
