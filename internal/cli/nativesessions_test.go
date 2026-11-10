package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/adapter/grok"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
	"github.com/congdoe/aggy/internal/transcript"
)

// Native session ids for the tests.
const (
	natA = "11111111-1111-4111-8111-111111111111"
	natB = "22222222-2222-4222-8222-222222222222"
	natC = "33333333-3333-4333-8333-333333333333"
	natD = "44444444-4444-4444-8444-444444444444"
)

// writeNativeAt writes content to path, making its folders, with modified time at.
func writeNativeAt(t *testing.T, path, content string, at time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// claudeUserLine is a Claude user line with text.
func claudeUserLine(text string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"content": text}})
	return string(b) + "\n"
}

// claudeLineIn is a Claude user line with text, recording cwd as its folder,
// as session lines do (docs/verification.md, "Claude Code session file fields").
func claudeLineIn(cwd, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "cwd": cwd, "message": map[string]any{"content": text}})
	return string(b) + "\n"
}

// claudeNativeAt writes a Claude session of cwd under home.
func claudeNativeAt(t *testing.T, home, cwd, id, prompt string, at time.Time) string {
	t.Helper()
	p := filepath.Join(home, ".claude", "projects", transcript.ClaudeProjectDir(cwd), id+".jsonl")
	writeNativeAt(t, p, claudeLineIn(cwd, prompt), at)
	return p
}

// codexRollout writes a Codex rollout of cwd under home, from source.
func codexNativeAt(t *testing.T, home, cwd, id, source, prompt string, at time.Time) {
	t.Helper()
	meta, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "source": source, "cwd": cwd}})
	msg, _ := json.Marshal(map[string]any{"type": "response_item", "payload": map[string]any{"type": "message", "role": "user",
		"content": []map[string]any{{"type": "input_text", "text": prompt}}}})
	day := at.UTC().Format("2006/01/02")
	name := "rollout-" + at.UTC().Format("2006-01-02T15-04-05") + "-" + id + ".jsonl"
	writeNativeAt(t, filepath.Join(home, ".codex", "sessions", day, name), string(meta)+"\n"+string(msg)+"\n", at)
}

// grokSessionAt writes a Grok session of cwd under home.
func grokSessionAt(t *testing.T, home, cwd, id, prompt string, at time.Time) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"type": "user", "content": []map[string]any{{"type": "text", "text": prompt}}})
	dir := filepath.Join(home, ".grok", "sessions", grok.EncodeCwd(cwd), id)
	writeNativeAt(t, filepath.Join(dir, "chat_history.jsonl"), string(b)+"\n", at)
	if err := os.Chtimes(dir, at, at); err != nil {
		t.Fatal(err)
	}
}

// nativeWorld is a temp HOME and a working folder in it, with the folder's
// canonical form, as the CLIs record it.
func nativeWorld(t *testing.T) (home, cwd string) {
	t.Helper()
	home = isolate(t)
	cwd = filepath.Join(home, "work")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd, _ = filepath.EvalSymlinks(cwd)
	return home, cwd
}

func TestClaudeDirName(t *testing.T) {
	tests := []struct {
		name       string
		cwd        string
		config     string
		project    string
		want       string
		wantListed bool
	}{
		{"a plain path", "/Users/me/my repo.x", "", "", "-Users-me-my-repo-x", true},
		{"a byte outside ASCII", "/Users/me/café", "", "", "", false},
		{"a name over 200", "/" + strings.Repeat("a", 200), "", "", "", false},
		{"exactly 200", "/" + strings.Repeat("a", 199), "", "", "-" + strings.Repeat("a", 199), true},
		{"a named project folder", "/x", "/cfg", "work_1", "work_1", true},
		{"a project name without the config dir", "/x", "", "work", "-x", true},
		{"a project name Claude ignores", "/x", "/cfg", "a/b", "-x", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := map[string]string{"CLAUDE_CONFIG_DIR": tt.config, "CLAUDE_CODE_PROJECT_DIR_NAME": tt.project}
			got, ok := claudeDirName(tt.cwd, func(k string) string { return env[k] })
			if ok != tt.wantListed || got != tt.want {
				t.Errorf("claudeDirName = %q, %v; want %q, %v", got, ok, tt.want, tt.wantListed)
			}
		})
	}
}

func TestNativeSessions(t *testing.T) {
	home, cwd := nativeWorld(t)
	other := filepath.Join(home, "other")
	now := time.Now().Truncate(time.Second)
	claudeNativeAt(t, home, cwd, natA, "fix the login bug", now.Add(-2*time.Hour))
	claudeNativeAt(t, home, cwd, natB, "newer one", now.Add(-time.Hour))
	claudeNativeAt(t, home, other, natC, "another folder", now)
	// Not a session: a name that is not an id, and a symlink to one.
	writeNativeAt(t, filepath.Join(home, ".claude", "projects", transcript.ClaudeProjectDir(cwd), "notes.jsonl"), claudeUserLine("x"), now)
	if err := os.Symlink(filepath.Join(home, ".claude", "projects", transcript.ClaudeProjectDir(other), natC+".jsonl"),
		filepath.Join(home, ".claude", "projects", transcript.ClaudeProjectDir(cwd), natD+".jsonl")); err != nil {
		t.Fatal(err)
	}
	codexNativeAt(t, home, cwd, natA, "cli", "codex here", now.Add(-3*time.Hour))
	codexNativeAt(t, home, cwd, natB, "exec", "a codex exec", now)
	codexNativeAt(t, home, other, natC, "cli", "codex elsewhere", now)
	grokSessionAt(t, home, cwd, natA, "grok here", now.Add(-time.Minute))
	grokSessionAt(t, home, other, natB, "grok elsewhere", now)

	tests := []struct {
		v      adapter.Vendor
		ids    []string
		titles []string
	}{
		{adapter.Claude, []string{natB, natA}, []string{"newer one", "fix the login bug"}},
		{adapter.Codex, []string{natA}, []string{"codex here"}},
		{adapter.Grok, []string{natA}, []string{"grok here"}},
		{adapter.OpenCode, nil, nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.v), func(t *testing.T) {
			ns, err := nativeSessions(&sessionFiles{}, tt.v, cwd)
			if err != nil {
				t.Fatal(err)
			}
			var ids, titles []string
			for _, n := range ns {
				ids, titles = append(ids, n.ID), append(titles, n.Title)
			}
			if strings.Join(ids, ",") != strings.Join(tt.ids, ",") || strings.Join(titles, "|") != strings.Join(tt.titles, "|") {
				t.Errorf("sessions = %v %q, want %v %q", ids, titles, tt.ids, tt.titles)
			}
		})
	}
}

// TestNativeTitleUntrusted: a first line with control characters lists as one
// escaped line, and one too large to read lists with no title.
func TestNativeTitleUntrusted(t *testing.T) {
	home, cwd := nativeWorld(t)
	now := time.Now()
	claudeNativeAt(t, home, cwd, natA, "\x1b[2J\x1b]52;c;aGk=\x07evil\r\nline\u202e", now)
	// The folder on a first line of its own (PR #127 review: a file whose head
	// records no folder is not listed), then a prompt too large to read.
	p := claudeNativeAt(t, home, cwd, natB, "", now.Add(-time.Minute))
	b, _ := json.Marshal(map[string]any{"type": "system", "cwd": cwd})
	writeNativeAt(t, p, string(b)+"\n"+claudeUserLine(strings.Repeat("x", nativeHead+10)), now.Add(-time.Minute))
	ns, err := nativeSessions(&sessionFiles{}, adapter.Claude, cwd)
	if err != nil || len(ns) != 2 {
		t.Fatalf("sessions = %+v, %v", ns, err)
	}
	out := renderChoices([]sessionChoice{{Vendor: adapter.Claude, When: now, Title: ns[0].Title, Native: &ns[0]},
		{Vendor: adapter.Claude, When: now, Title: ns[1].Title, Native: &ns[1]}}, now)
	if strings.ContainsAny(out, "\x1b\x07\r\u202e") || strings.Count(out, "\n") != 3 {
		t.Errorf("list = %q, want three plain lines", out)
	}
	if !strings.Contains(out, "(no prompt yet)") {
		t.Errorf("list = %q, want the huge first line shown as none", out)
	}
}

func TestSessionTitle(t *testing.T) {
	tests := []struct{ in, want string }{
		{"fix  the\n\tbug", "fix the bug"},
		{"\x1b[31mred", `\x1b[31mred`},
		{strings.Repeat("é", 80), strings.Repeat("é", maxTitle-1) + "…"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := sessionTitle(tt.in); got != tt.want {
			t.Errorf("sessionTitle(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestAdoptOnce (T6965): a native session reopened through aggy gets a parent
// and a child once; reopening it again finds that child.
func TestAdoptOnce(t *testing.T) {
	home, cwd := nativeWorld(t)
	claudeNativeAt(t, home, cwd, natA, "hello", time.Now().Add(-time.Hour))
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var errb strings.Builder
	for i := range 2 {
		ns, ok := findNative(&sessionFiles{}, []adapter.Vendor{adapter.Claude}, cwd, natA)
		if !ok {
			t.Fatal("not found")
		}
		p, err := plan(ctx, s, config.Default(), cwd, target{Native: &ns}, time.Now(), &errb)
		if err != nil {
			t.Fatal(err)
		}
		if p.resume != natA || p.child.VendorSessionID != natA {
			t.Fatalf("plan %d resumes %q", i, p.resume)
		}
		if (i == 0) != (p.newParent && p.newChild && p.adopted) {
			t.Fatalf("plan %d: new parent %v, new child %v, adopted %v", i, p.newParent, p.newChild, p.adopted)
		}
		lease, err := p.commit(ctx, s, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if lease != nil {
			_ = lease.Release(ctx)
		}
	}
	ps, err := s.Parents(ctx)
	if err != nil || len(ps) != 1 {
		t.Fatalf("parents = %d, %v; want 1", len(ps), err)
	}
	cs, _ := s.Children(ctx, ps[0].ID)
	if len(cs) != 1 || cs[0].Title != "hello" {
		t.Errorf("children = %+v, want one titled hello", cs)
	}
}

// TestContinueNative (T6965): -c takes the newer of aggy's latest session here
// and the vendor's own.
func TestContinueNative(t *testing.T) {
	home, cwd := nativeWorld(t)
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	var errb strings.Builder
	// aggy's own session, last on screen an hour ago.
	then := time.Now().Add(-time.Hour)
	claudeNativeAt(t, home, cwd, natA, "aggy's", then)
	ns, _ := findNative(&sessionFiles{}, []adapter.Vendor{adapter.Claude}, cwd, natA)
	p, err := plan(ctx, s, config.Default(), cwd, target{Native: &ns}, then, &errb)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := p.commit(ctx, s, then)
	if err != nil {
		t.Fatal(err)
	}
	_ = lease.Release(ctx)

	tests := []struct {
		name   string
		native time.Duration // age of a native session; 0 for none
		want   string
		adopt  bool
	}{
		{"older native", 2 * time.Hour, natA, false},
		{"newer native", time.Minute, natB, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claudeNativeAt(t, home, cwd, natB, "native", time.Now().Add(-tt.native))
			p, err := plan(ctx, s, config.Default(), cwd, target{Continue: true, Vendor: adapter.Claude}, time.Now(), &errb)
			if err != nil {
				t.Fatal(err)
			}
			if p.resume != tt.want || p.adopted != tt.adopt {
				t.Errorf("resume %q adopted %v, want %q %v", p.resume, p.adopted, tt.want, tt.adopt)
			}
		})
	}
}

func TestResolveResume(t *testing.T) {
	home, cwd := nativeWorld(t)
	t.Chdir(cwd)
	t.Setenv("PATH", t.TempDir())
	claudeNativeAt(t, home, cwd, natA, "here", time.Now())
	claudeNativeAt(t, home, filepath.Join(home, "other"), natB, "there", time.Now())
	tests := []struct {
		name   string
		v      adapter.Vendor
		value  string
		code   diag.Code
		native string
		byName string
	}{
		{"a native id here", adapter.Claude, natA, diag.Code{}, natA, ""},
		{"a native id with no vendor named", "", natA, diag.Code{}, natA, ""},
		{"an id of another folder", adapter.Claude, natB, diag.CodeNotFound, "", ""},
		{"a name for claude", adapter.Claude, "auth-refactor", diag.Code{}, "", "auth-refactor"},
		{"a name with spaces", adapter.Grok, "My Project", diag.Code{}, "", "My Project"},
		{"a name for codex", adapter.Codex, "auth-refactor", diag.CodeNotFound, "", ""},
		{"a path", adapter.Claude, "../x", diag.CodeUsage, "", ""},
		{"an absolute path", adapter.Claude, "/tmp/s.jsonl", diag.CodeUsage, "", ""},
		{"a flag", adapter.Claude, "--dangerously-skip-permissions", diag.CodeUsage, "", ""},
		{"a control character", adapter.Claude, "a\x1bb", diag.CodeUsage, "", ""},
		{"too long", adapter.Claude, strings.Repeat("n", adapter.MaxResumeName+1), diag.CodeUsage, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tg := target{Vendor: tt.v}
			code, err := resolveResume(&tg, tt.v, tt.value)
			if code != tt.code || (err != nil) != (tt.code != diag.Code{}) {
				t.Fatalf("code %v, err %v; want %v", code, err, tt.code)
			}
			if tt.code == diag.CodeNotFound && !errors.Is(err, errNoSession) {
				t.Errorf("err = %v, want errNoSession", err)
			}
			got := ""
			if tg.Native != nil {
				got = tg.Native.ID
			}
			if got != tt.native || tg.ResumeName != tt.byName {
				t.Errorf("native %q name %q, want %q %q", got, tg.ResumeName, tt.native, tt.byName)
			}
		})
	}
}

// TestPickSession (T6965): -r lists aggy's and the vendor's sessions here,
// newest first, and takes the one picked.
func TestPickSession(t *testing.T) {
	home, cwd := nativeWorld(t)
	t.Chdir(cwd)
	t.Setenv("PATH", t.TempDir())
	claudeNativeAt(t, home, cwd, natA, "older", time.Now().Add(-2*time.Hour))
	codexNativeAt(t, home, cwd, natB, "cli", "newer", time.Now().Add(-time.Hour))
	tests := []struct {
		answer string
		picked bool
		id     string
		code   diag.Code
	}{
		{"1\n", true, natB, diag.Code{}},
		{"2\n", true, natA, diag.Code{}},
		{"\n", false, "", diag.Code{}},
		{"9\n", false, "", diag.CodeUsage},
	}
	for _, tt := range tests {
		t.Run(strings.TrimSpace(tt.answer), func(t *testing.T) {
			pickIn = strings.NewReader(tt.answer)
			defer func() { pickIn = os.Stdin }()
			defer func(f func() bool) { stdinTerminal = f }(stdinTerminal)
			stdinTerminal = func() bool { return true }
			var out strings.Builder
			var tg target
			picked, code, _ := pickSession(&tg, "", &out)
			if picked != tt.picked || code != tt.code {
				t.Fatalf("picked %v code %v, want %v %v; list %q", picked, code, tt.picked, tt.code, out.String())
			}
			if tt.picked && (tg.Native == nil || tg.Native.ID != tt.id) {
				t.Errorf("picked %+v, want %s", tg.Native, tt.id)
			}
			list := out.String()
			if i, j := strings.Index(list, " newer\n"), strings.Index(list, " older\n"); i < 0 || j < i {
				t.Errorf("list = %q, want newer first", list)
			}
		})
	}
}

// TestAdoptNamed (T6965, PR #127 review): a session resumed by name gets the
// id the CLI's SessionStart hook reported, when that session is this folder's;
// otherwise neither its child nor its parent is kept.
func TestAdoptNamed(t *testing.T) {
	tests := []struct {
		name     string
		reported string
		write    []string // sessions written in the run
		held     bool     // the reported session is another child's already
		want     string   // the id recorded, or "" for the rows dropped
		say      string
	}{
		{"reported and here", natA, []string{natA}, false, natA, "is aggy's session"},
		{"reported, another changed too", natA, []string{natA, natB}, false, natA, "is aggy's session"},
		{"nothing reported, one changed", "", []string{natA}, false, "", "did not say"},
		{"reported but not here", natD, []string{natA}, false, "", "not this folder's"},
		{"reported and held", natC, nil, true, "", "already"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			home, cwd := nativeWorld(t)
			ctx := context.Background()
			s, err := store.OpenDefault(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			claudeNativeAt(t, home, cwd, natC, "old", time.Now().Add(-time.Hour))
			var errb strings.Builder
			if tt.held {
				ns, _ := findNative(&sessionFiles{}, []adapter.Vendor{adapter.Claude}, cwd, natC)
				p, err := plan(ctx, s, config.Default(), cwd, target{Native: &ns}, time.Now(), &errb)
				if err != nil {
					t.Fatal(err)
				}
				l, err := p.commit(ctx, s, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				_ = l.Release(ctx)
			}
			p, err := plan(ctx, s, config.Default(), cwd, target{ResumeName: "auth", Vendor: adapter.Claude}, time.Now(), &errb)
			if err != nil {
				t.Fatal(err)
			}
			if p.resumeName != "auth" || p.child.VendorSessionID != "" {
				t.Fatalf("plan = %+v", p)
			}
			if _, err := p.commit(ctx, s, time.Now()); err != nil {
				t.Fatal(err)
			}
			for _, id := range tt.write {
				claudeNativeAt(t, home, cwd, id, "x", time.Now())
			}
			var out strings.Builder
			adoptNamed(ctx, s, p.child, tt.reported, &sessionFiles{}, &out)
			if !strings.Contains(out.String(), tt.say) {
				t.Errorf("said %q, want %q", out.String(), tt.say)
			}
			cs, _ := s.Children(ctx, p.parent.ID)
			if tt.want == "" {
				if _, err := s.GetParent(ctx, p.parent.ID); len(cs) != 0 || !errors.Is(err, store.ErrNotFound) {
					t.Errorf("children %+v, parent %v; want both dropped", cs, err)
				}
				return
			}
			if len(cs) != 1 || cs[0].VendorSessionID != tt.want {
				t.Errorf("children = %+v, want the id %q", cs, tt.want)
			}
		})
	}
}

// TestNamedSessionHook (PR #127 review): only the first SessionStart from the
// session's own process tree, for the vendor on screen, names the session.
func TestNamedSessionHook(t *testing.T) {
	pay := func(id string) []byte { return []byte(`{"session_id":"` + id + `","source":"resume"}`) }
	tests := []struct {
		name string
		reqs []supervisor.Request
		want string
	}{
		{"first", []supervisor.Request{{Vendor: "claude", Event: "SessionStart", Payload: pay(natA), FromSession: true},
			{Vendor: "claude", Event: "SessionStart", Payload: pay(natB), FromSession: true}}, natA},
		{"not from the session", []supervisor.Request{{Vendor: "claude", Event: "SessionStart", Payload: pay(natA)}}, ""},
		{"another event", []supervisor.Request{{Vendor: "claude", Event: "Stop", Payload: pay(natA), FromSession: true}}, ""},
		{"another vendor", []supervisor.Request{{Vendor: "grok", Event: "SessionStart", Payload: pay(natA), FromSession: true}}, ""},
		{"not an id", []supervisor.Request{{Vendor: "claude", Event: "SessionStart", Payload: pay("-x"), FromSession: true},
			{Vendor: "claude", Event: "SessionStart", Payload: pay(natB), FromSession: true}}, ""},
	}
	for _, tt := range tests {
		n := &namedSession{vendor: adapter.Claude}
		for _, r := range tt.reqs {
			n.hook(r)
		}
		if got := n.reported(); got != tt.want {
			t.Errorf("%s: reported %q, want %q", tt.name, got, tt.want)
		}
	}
}
