package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/config"
	"github.com/congdoe/aggy/internal/permissions"
)

// setPolicy writes aggy's permissions file with settings for every repo.
func setPolicy(t *testing.T, settings map[string]string) {
	t.Helper()
	path, err := permissionsFile()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"policy": settings})
	writeAt(t, path, string(body), 0o600)
}

// TestGroupOffKeepsHook (T6979): with every group off there is no policy ask
// left, but the decision hook still goes on, for aggy's own files and the
// static asks on Claude's settings files, which turning a group off never
// turns off. With a group off and the hook unable to run, the note says what
// is lost, not that the policy asks as before.
func TestGroupOffKeepsHook(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	fakeHookBin(t, "/usr/local/bin/aggy", nil)
	all := map[string]string{}
	for _, g := range permissions.PolicyGroups {
		all[g] = "allow"
	}
	setPolicy(t, all)
	req, plan, s := onScreenReq(t, repo)
	if len(req.Ask) != 0 || len(req.Deny) != 0 {
		t.Fatalf("every group off still renders: ask %q deny %q", req.Ask, req.Deny)
	}
	if note := usePolicyHook(&req, hookIn(plan, s, repo, repo, "", filepath.Join(t.TempDir(), "m"))); note != "" || req.PolicyHook == nil {
		t.Fatalf("no hook: %q", note)
	}
	if !slices.Equal(req.Ask, settingsGuardAsks(repo, repo)) {
		t.Errorf("settings asks %q", req.Ask)
	}
	path, sum := planOf(t, req)
	d, err := readDecidePlan(path, sum)
	if err != nil || len(d.Protected) == 0 || d.Settings[permissions.PolicyRmRf] != "allow" {
		t.Fatalf("plan %+v %v", d, err)
	}

	req, plan, s = onScreenReq(t, repo)
	req.Extra = []string{"--safe-mode"}
	if note := usePolicyHook(&req, hookIn(plan, s, repo, repo, "", filepath.Join(t.TempDir(), "m"))); req.PolicyHook != nil || !strings.Contains(note, "aggy's asks on writes to its own files") {
		t.Errorf("--safe-mode: %q", note)
	}

	// Nothing off and nothing asking (every group denied): no hook, as before.
	deny := map[string]string{}
	for _, g := range permissions.PolicyGroups {
		deny[g] = "deny"
	}
	setPolicy(t, deny)
	req, plan, s = onScreenReq(t, repo)
	if note := usePolicyHook(&req, hookIn(plan, s, repo, repo, "", filepath.Join(t.TempDir(), "m"))); note != "" || req.PolicyHook != nil {
		t.Errorf("all denied: %q %+v", note, req.PolicyHook)
	}
}

// TestPolicyLaunchLines (T6979): the launch's lines while a group is off, for
// a repo's own [policy] table, and for Grok and Codex.
func TestPolicyLaunchLines(t *testing.T) {
	off := permissions.Set{Policy: map[string]permissions.Kind{permissions.PolicyRmRf: permissions.Allow, permissions.PolicyKeys: permissions.Allow, permissions.PolicyEnv: permissions.Ask}}
	all := permissions.Set{Policy: map[string]permissions.Kind{}}
	for _, g := range permissions.PolicyGroups {
		all.Policy[g] = permissions.Allow
	}
	for _, c := range []struct {
		name string
		got  string
		want string // "" for no line
	}{
		{"none off", policyOffLine(permissions.Set{}), ""},
		{"two off", policyOffLine(off), "the safety policy is off on screen for key-read, rm-rf"},
		{"repo sets none", repoPolicyLine(config.RepoConfig{}), ""},
		{"repo sets [policy]", repoPolicyLine(config.RepoConfig{Policy: map[string]any{"rm-rf": "allow"}}), "[policy] table, which aggy ignores"},
		{"grok", policyVendorLine(adapter.Grok, permissions.Set{}), "is refused, with no prompt"},
		{"grok ways forward", policyVendorLine(adapter.Grok, off), "do it on Claude Code or OpenCode, or turn its group off with aggy permissions policy GROUP allow"},
		{"grok, every group off", policyVendorLine(adapter.Grok, all), ""},
		{"codex", policyVendorLine(adapter.Codex, permissions.Set{}), "asks before each match, every time, and an exact pre-approval (aggy permissions add allow) does not apply"},
		{"codex, every group off", policyVendorLine(adapter.Codex, all), "not on Codex, whose rules file is global"},
		{"claude", policyVendorLine(adapter.Claude, permissions.Set{}), ""},
		{"opencode", policyVendorLine(adapter.OpenCode, permissions.Set{}), ""},
	} {
		if c.want == "" && c.got != "" || c.want != "" && !strings.Contains(c.got, c.want) {
			t.Errorf("%s: %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestPolicyOffRepoKeys: of two keys naming one repo, the stricter setting
// wins, allow being the least strict.
func TestPolicyOffRepoKeys(t *testing.T) {
	for _, c := range []struct {
		name string
		a, b string
		want permissions.Kind
	}{
		{"allow and ask", "allow", "ask", permissions.Ask},
		{"ask and allow", "ask", "allow", permissions.Ask},
		{"allow and deny", "allow", "deny", permissions.Deny},
		{"allow and allow", "allow", "allow", permissions.Allow},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			repo := inRepo(t)
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(repo, link); err != nil {
				t.Fatal(err)
			}
			path, err := permissionsFile()
			if err != nil {
				t.Fatal(err)
			}
			for _, order := range [][2]string{{repo, link}, {link, repo}} {
				body, _ := json.Marshal(map[string]any{"repos": map[string]any{
					order[0]: map[string]any{"policy": map[string]string{permissions.PolicyRmRf: c.a}},
					order[1]: map[string]any{"policy": map[string]string{permissions.PolicyRmRf: c.b}},
				}})
				writeAt(t, path, string(body), 0o600)
				s, _, _, err := permissionsHere(t.Context())
				if err != nil || s.FileErr != nil {
					t.Fatal(err, s.FileErr)
				}
				if s.Policy[permissions.PolicyRmRf] != c.want {
					t.Errorf("%v: %q, want %q", order, s.Policy[permissions.PolicyRmRf], c.want)
				}
			}
		})
	}
}
