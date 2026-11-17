package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/checkup"
	"github.com/congdoe/aggy/internal/probe"
)

// TestLaunchStatus (T7015): what a launch no longer says on screen is in aggy
// doctor's section for the folder, from the same functions: how the policy
// works on each CLI, a repo's ignored tables, rules that do not reach a CLI,
// exact pre-approvals, and aggy's command skills missing from a CLI's menu.
// A message holds no path or rule text, so a report can carry it.
func TestLaunchStatus(t *testing.T) {
	installed := func(vs ...adapter.Vendor) []checkup.Vendor {
		var out []checkup.Vendor
		for _, v := range vs {
			out = append(out, checkup.Vendor{Vendor: v, Installed: true, Capabilities: []string{probe.CapSettings, probe.CapAllow, probe.CapDeny}})
		}
		return out
	}
	for _, c := range []struct {
		name    string
		setup   func(t *testing.T, repo string)
		vendors []checkup.Vendor
		code    string
		vendor  adapter.Vendor
		want    string
	}{
		{"codex's policy", nil, installed(adapter.Codex), codePolicyOnVendor, adapter.Codex, "on Codex, aggy's safety policy asks before each match"},
		{"grok's policy", nil, installed(adapter.Grok), codePolicyOnVendor, adapter.Grok, "is refused, with no prompt"},
		{"hermes' policy", nil, installed(adapter.Hermes), codePolicyOnVendor, adapter.Hermes, "on Hermes Agent"},
		{"antigravity's policy", nil, installed(adapter.Antigravity), codePolicyOnVendor, adapter.Antigravity, "on Antigravity"},
		{"repo [policy]", func(t *testing.T, repo string) {
			writeAt(t, filepath.Join(repo, ".aggy", "config.toml"), "[policy]\nrm-rf = \"allow\"\n", 0o600)
		}, nil, codeRepoIgnored, "", "[policy] table, which aggy ignores"},
		{"repo [keys]", func(t *testing.T, repo string) {
			writeAt(t, filepath.Join(repo, ".aggy", "config.toml"), "[keys]\nanthropic = \"sk-x\"\n", 0o600)
		}, nil, codeRepoIgnored, "", "[keys]"},
		{"a rule that does not apply", func(t *testing.T, repo string) {
			writePermissions(t, `{"permissions":{"allow":["Bash(*)"]}}`)
		}, installed(adapter.Claude), codePermRules, adapter.Claude, "1 of your permission rules does not apply on Claude Code"},
		{"an allow naming a policy rule", func(t *testing.T, repo string) {
			atKeyboard(t, "1\n\n")
			if code, out, errb := run("trust", repo); code != 0 {
				t.Fatalf("trust: %d %q %q", code, out, errb)
			}
			atKeyboard(t, "y\n")
			if code, out, errb := run("permissions", "add", "allow", "Bash(git push --force origin main)"); code != 0 {
				t.Fatalf("add: %d %q %q", code, out, errb)
			}
		}, installed(adapter.Grok), codePermRules, adapter.Grok, "an allow of yours names a safety policy rule, so it does not go to Grok"},
		{"an exact pre-approval", func(t *testing.T, repo string) {
			atKeyboard(t, "1\n\n")
			if code, out, errb := run("trust", repo); code != 0 {
				t.Fatalf("trust: %d %q %q", code, out, errb)
			}
			atKeyboard(t, "y\n")
			if code, out, errb := run("permissions", "add", "allow", "Bash(git push --force origin main)"); code != 0 {
				t.Fatalf("add: %d %q %q", code, out, errb)
			}
		}, installed(adapter.Claude), codePreApprovals, adapter.Claude, "1 exact pre-approval of a safety policy match applies in Claude Code sessions here"},
		{"pre-approvals with hooks off", func(t *testing.T, repo string) {
			atKeyboard(t, "1\n\n")
			if code, out, errb := run("trust", repo); code != 0 {
				t.Fatalf("trust: %d %q %q", code, out, errb)
			}
			atKeyboard(t, "y\n")
			if code, out, errb := run("permissions", "add", "allow", "Bash(git push --force origin main)"); code != 0 {
				t.Fatalf("add: %d %q %q", code, out, errb)
			}
			writeAt(t, filepath.Join(repo, ".claude", "settings.json"), `{"disableAllHooks": true}`, 0o600)
		}, installed(adapter.Claude), codePreApprovals, adapter.Claude, "turns Claude's hooks off, so exact pre-approvals do not apply here"},
		{"command skills missing", nil, installed(adapter.Claude), codeCommandSkills, adapter.Claude, "are not in claude's menu"},
	} {
		t.Run(c.name, func(t *testing.T) {
			isolate(t)
			repo := inRepo(t)
			if c.setup != nil {
				c.setup(t, repo)
			}
			var got []string
			for _, f := range launchStatus(t.Context(), repo, c.vendors) {
				if f.Level != checkup.LevelInfo {
					t.Errorf("%s is %s, want info", f.Code, f.Level)
				}
				if strings.Contains(f.Message, repo) || strings.Contains(f.Message, "Bash(") {
					t.Errorf("a path or rule in the message: %q", f.Message)
				}
				if f.Code == c.code && f.Vendor == c.vendor {
					got = append(got, f.Message)
				}
			}
			if !strings.Contains(strings.Join(got, "\n"), c.want) {
				t.Errorf("%s findings %q, want %q", c.code, got, c.want)
			}
		})
	}
}

// TestLaunchStatusNoneHere (T7015): a CLI that is not installed gets no
// finding, and an ordinary folder with nothing to say gets none of the
// policy's or the rules'.
func TestLaunchStatusNoneHere(t *testing.T) {
	isolate(t)
	repo := inRepo(t)
	for _, f := range launchStatus(t.Context(), repo, []checkup.Vendor{{Vendor: adapter.Codex}, {Vendor: adapter.Claude, Installed: true, Capabilities: []string{probe.CapSettings}}}) {
		if f.Vendor == adapter.Codex || f.Code == codePermRules || f.Code == codePreApprovals || f.Code == codeRepoIgnored || f.Code == codePolicyOnVendor {
			t.Errorf("finding %s (%s): %q", f.Code, f.Vendor, f.Message)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".aggy")); err == nil {
		t.Error("doctor made the repo's .aggy folder")
	}
}
