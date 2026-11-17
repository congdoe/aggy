package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/permissions"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/safefs"
	"github.com/congdoe/aggy/internal/store"
)

// TestMemoryMissedKind (#148 review M2): a launch that does not deliver the
// shared memory knows which kind of reason, for its diagnostic record, which
// holds no error text.
func TestMemoryMissedKind(t *testing.T) {
	for _, c := range []struct {
		name   string
		v      adapter.Vendor
		caps   []string
		memory string
		memDir func(t *testing.T) string
		want   string
	}{
		{"delivered", adapter.Claude, nil, "note", func(t *testing.T) string { return filepath.Join(t.TempDir(), "m") }, ""},
		{"no memory", adapter.Claude, nil, "", func(t *testing.T) string { return filepath.Join(t.TempDir(), "m") }, ""},
		{"too big", adapter.Claude, nil, strings.Repeat("x", parity.MemoryCap+1), func(t *testing.T) string { return filepath.Join(t.TempDir(), "m") }, "too_big"},
		{"no flag", adapter.Grok, nil, "note", func(t *testing.T) string { return filepath.Join(t.TempDir(), "m") }, "no_flag"},
		{"cannot write", adapter.Claude, nil, "note", func(t *testing.T) string { return filepath.Join(t.TempDir(), "gone", "m") }, "write_failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			root := tempRepo(t)
			if c.memory != "" {
				writeAt(t, parity.MemoryPath(root), c.memory, 0o600)
			}
			var req adapter.InteractiveRequest
			cleanup, _, why := addMemoryWhy(c.v, c.caps, root, c.memDir(t), &req)
			defer cleanup()
			if why != c.want {
				t.Errorf("why %q, want %q", why, c.want)
			}
		})
	}
}

// TestNewPreApprovalsLine (#148 review M1): a launch says when exact
// pre-approvals of policy matches apply that the last launch in the folder
// did not, once; an unchanged set, or one that only shrank, says nothing; one
// removed and added again is new again. The record holds no rule text.
func TestNewPreApprovalsLine(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	s, err := store.OpenDefault(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	push, force := "git push --force origin main", "git push --force origin dev"
	keys := func(cmds ...string) []string {
		return preApprovalKeys(parity.PreApproved{Commands: cmds}, nil)
	}
	if len(keys(push, force, "ls")) != 2 {
		t.Fatalf("keys %q: want the two policy matches only", keys(push, force, "ls"))
	}
	for i, c := range []struct {
		set  []string
		want string
	}{
		{keys(push), "1 new exact pre-approval of a safety policy match applies here"},
		{keys(push), ""},
		{keys(push, force), "1 new exact pre-approval"},
		{keys(push, force), ""},
		{keys(force), ""},
		{keys(push, force), "1 new exact pre-approval"},
		{nil, ""},
		{keys(push, force), "2 new exact pre-approvals of safety policy matches apply here"},
	} {
		got := newPreApprovalsLine(t.Context(), s, repo, c.set)
		if c.want == "" && got != "" || !strings.Contains(got, c.want) {
			t.Errorf("launch %d: %q, want %q", i, got, c.want)
		}
	}
	key, _ := safefs.CanonicalPath(repo)
	if v, _, _ := s.Note(t.Context(), preApprovalsSeenNote+key); strings.Contains(v, "git") {
		t.Errorf("the record holds rule text: %q", v)
	}
}

// TestSkippedProtectionsOnScreen (#148 review H2): a deny or ask of the
// person's that does not reach the CLI is said at launch; skipped allows, and
// the allows a lost rule takes with it, are aggy doctor's only.
func TestSkippedProtectionsOnScreen(t *testing.T) {
	caps := []string{probe.CapSettings, probe.CapAllow, probe.CapDeny, probe.CapPermissionMode}
	rule := func(k permissions.Kind, text string) permissions.Rule {
		return permissions.Rule{Kind: k, Text: text, Origin: permissions.FromAggy}
	}
	for _, c := range []struct {
		name   string
		v      adapter.Vendor
		rules  []permissions.Rule
		screen string // "" for no line
		status string // "" for no status note
	}{
		{"grok, denies and an ask", adapter.Grok, []permissions.Rule{
			rule(permissions.Deny, "WebSearch"), rule(permissions.Deny, "Bash(rm -rf a,b)"), rule(permissions.Ask, "Bash(git push *)"),
		}, "3 of your deny or ask rules do not reach " + vendorNames[adapter.Grok], ""},
		{"grok, one ask and an allow it takes", adapter.Grok, []permissions.Rule{
			rule(permissions.Ask, "Bash(git push *)"), rule(permissions.Allow, "Bash(make build)"),
		}, "1 of your deny or ask rules does not reach " + vendorNames[adapter.Grok], "none of your allow rules from aggy reach Grok"},
		{"grok, allows only", adapter.Grok, []permissions.Rule{
			rule(permissions.Allow, "Bash(*)"),
		}, "", "1 of your permission rules does not apply on " + vendorNames[adapter.Grok]},
		{"claude, everything reaches", adapter.Claude, []permissions.Rule{
			rule(permissions.Deny, "WebSearch"), rule(permissions.Ask, "Bash(git push *)"),
		}, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			var req adapter.InteractiveRequest
			rn := sharedRules(c.v, caps, permissions.Set{Trusted: true, Rules: c.rules}, &req)
			screen := strings.Join(rn.screen, "\n")
			if c.screen == "" && screen != "" || !strings.Contains(screen, c.screen) {
				t.Errorf("screen %q, want %q", screen, c.screen)
			}
			if st := statusText(rn); c.status == "" && st != "" || !strings.Contains(st, c.status) {
				t.Errorf("status %q, want %q", st, c.status)
			}
		})
	}
}

// TestGrokModeNotPassed (#148 review M3): a Grok without --permission-mode
// in its help gets no mode from aggy; a mode stricter than its default is said
// at launch, a looser one is aggy doctor's.
func TestGrokModeNotPassed(t *testing.T) {
	for _, c := range []struct {
		mode   string
		screen bool
	}{
		{"plan", true},
		{"dontAsk", true},
		{"auto", false},
		{"acceptEdits", false},
	} {
		t.Run(c.mode, func(t *testing.T) {
			var req adapter.InteractiveRequest
			rn := sharedRules(adapter.Grok, []string{probe.CapAllow, probe.CapDeny}, permissions.Set{Trusted: true, Mode: c.mode, ModeFrom: permissions.FromConfig}, &req)
			on := strings.Contains(strings.Join(rn.screen, "\n"), "--permission-mode")
			doc := strings.Contains(statusText(rn), "--permission-mode")
			if on != c.screen || doc == c.screen || req.Rules.Mode != "" {
				t.Errorf("screen %v, doctor %v, mode %q: %q %q", on, doc, req.Rules.Mode, rn.screen, statusText(rn))
			}
		})
	}
}

// TestPrintModeNotes (#148 review L4): print mode prints every rule note in
// the words and order it did before T7015.
func TestPrintModeNotes(t *testing.T) {
	s := permissions.Set{Trusted: true, Mode: "auto", ModeFrom: permissions.FromConfig,
		Rules: []permissions.Rule{
			{Kind: permissions.Allow, Text: "Bash(gh pr list)", Origin: permissions.FromAggy},
			{Kind: permissions.Ask, Text: "Bash(git push *)", Origin: permissions.FromAggy},
		},
		Problems: []permissions.Problem{{Path: "/p", Why: "broken"}},
	}
	var req adapter.InteractiveRequest
	got := sharedRules(adapter.Grok, nil, s, &req).all
	want := []string{
		"a deny may be missing, from /p: broken",
		"none of your allow rules from aggy reach Grok this session: the rule Bash(git push *) cannot reach it, and it might have stopped one",
		"2 of your permission rules do not apply on " + vendorNames[adapter.Grok] + "; aggy permissions show says why",
		"grok --help does not list --permission-mode, so the mode set in aggy does not reach this session",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("print mode's notes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestOpenCodeNoteFolder (#148 review L2): outside a repo the untrusted note's
// record names the launch folder, canonical, so one folder's record does not
// silence another's.
func TestOpenCodeNoteFolder(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	ca, _ := safefs.CanonicalPath(a)
	if got := openCodeNoteFolder("", a); got != ca {
		t.Errorf("outside a repo: %q, want %q", got, ca)
	}
	if openCodeNoteFolder("", a) == openCodeNoteFolder("", b) {
		t.Error("two folders outside a repo share one record")
	}
	if got := openCodeNoteFolder("/r", a); got != "/r" {
		t.Errorf("in a repo: %q", got)
	}
}

// TestAgyHookSeen (#148 review H3): agy's trust note is said in a folder
// until a hook of agy's has come from it, keyed by the canonical folder.
func TestAgyHookSeen(t *testing.T) {
	isolate(t)
	s, err := store.OpenDefault(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if agyHookSeen(t.Context(), s, dir) {
		t.Fatal("seen before any hook")
	}
	markAgyHookSeen(t.Context(), s, link)
	if !agyHookSeen(t.Context(), s, dir) {
		t.Error("a hook from the folder, by a link to it, not recorded for it")
	}
	if agyHookSeen(t.Context(), s, t.TempDir()) {
		t.Error("another folder counts as seen")
	}
}

// TestOpenCodeSkippedProtections (#148 review H2): OpenCode follows the same
// rule: an ask that cannot reach it is said at launch, an allow that cannot
// is not.
func TestOpenCodeSkippedProtections(t *testing.T) {
	for _, c := range []struct {
		name string
		rule permissions.Rule
		want string // "" for no line
	}{
		{"an ask it cannot take", permissions.Rule{Kind: permissions.Ask, Text: "WebSearch", Origin: permissions.FromAggy}, "1 of your deny or ask rules does not reach OpenCode"},
		{"an allow it cannot take", permissions.Rule{Kind: permissions.Allow, Text: "WebSearch", Origin: permissions.FromAggy}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo, dir := openCodeWorld(t)
			s, err := prepareOpenCode(openCodeInput{cfg: effective(config.Default(), config.RepoConfig{}), caps: openCodeCaps, cwd: repo, dir: dir,
				rules: permissions.Set{Trusted: true, Rules: []permissions.Rule{c.rule}}})
			if err != nil {
				t.Fatal(err)
			}
			warns := strings.Join(s.warns, "\n")
			if strings.Contains(warns, "permission rules") || c.want == "" && strings.Contains(warns, "deny or ask") || !strings.Contains(warns, c.want) {
				t.Errorf("warns %q, want %q", warns, c.want)
			}
		})
	}
}
