package parity

import (
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/permissions"
)

// TestDecideCall (T6976): the one decision. A policy match asks unless the
// whole call is exactly pre-approved; a group set to deny denies whatever is
// pre-approved; no match has no opinion. Every spelling that is not the
// pre-approved bytes asks: whitespace, quoting, a continuation, a look-alike
// letter, a comment, a compound command with the approved part in it.
func TestDecideCall(t *testing.T) {
	const force = "git push --force origin main"
	pre := PreApproved{Commands: []string{force, "rm -rf dist"}, Reads: []string{"/repo/certs/server.pem"}}
	on := PolicyPlan{OnScreen: true}
	denyForce := PolicyPlan{OnScreen: true, Settings: map[string]permissions.Kind{permissions.PolicyForce: permissions.Deny, permissions.PolicyForceMain: permissions.Deny}}
	shell := func(s string) Call { return Call{Kind: CallShell, Text: s} }
	tests := []struct {
		name  string
		plan  PolicyPlan
		call  Call
		want  Verdict
		group string
	}{
		{"exact pre-approval", on, shell(force), VerdictAllow, permissions.PolicyPushMain},
		{"exact, not strict", on, shell("rm -rf dist"), VerdictAllow, permissions.PolicyRmRf},
		{"no match", on, shell("git status"), VerdictNone, ""},
		{"no match, a word inside another", on, shell("echo reset --hardly"), VerdictNone, ""},
		{"not pre-approved", on, shell("git push --force origin dev"), VerdictAsk, permissions.PolicyForce},
		{"group set to deny wins over a pre-approval", denyForce, shell(force), VerdictDeny, permissions.PolicyForce},
		{"double space", on, shell("git push  --force origin main"), VerdictAsk, permissions.PolicyPushMain},
		{"leading space", on, shell(" " + force), VerdictAsk, permissions.PolicyPushMain},
		{"trailing space", on, shell(force + " "), VerdictAsk, permissions.PolicyPushMain},
		{"tab", on, shell("git push\t--force origin main"), VerdictAsk, permissions.PolicyPushMain},
		{"quoted", on, shell(`git push --force origin "main"`), VerdictAsk, permissions.PolicyPushMain},
		{"quoted flag", on, shell(`git push "--force" origin main`), VerdictAsk, permissions.PolicyPushMain},
		{"continuation", on, shell("git push --force \\\norigin main"), VerdictAsk, permissions.PolicyPushMain},
		{"split word continuation", on, shell("git push --fo\\\nrce origin main"), VerdictAsk, permissions.PolicyPushMain},
		{"look-alike letter", on, shell("git push --force origin mаin"), VerdictAsk, permissions.PolicyForce},
		{"comment", on, shell(force + " # ok"), VerdictAsk, permissions.PolicyPushMain},
		{"and", on, shell(force + " && echo done"), VerdictAsk, permissions.PolicyPushMain},
		{"and before", on, shell("true && " + force), VerdictAsk, permissions.PolicyPushMain},
		{"semicolon", on, shell(force + "; rm -rf ~"), VerdictAsk, permissions.PolicyPushMain},
		{"pipe", on, shell("yes | " + force), VerdictAsk, permissions.PolicyPushMain},
		{"newline", on, shell(force + "\nrm -rf /"), VerdictAsk, permissions.PolicyPushMain},
		{"subshell", on, shell("(" + force + ")"), VerdictAsk, permissions.PolicyPushMain},
		{"substitution", on, shell("echo $(" + force + ")"), VerdictAsk, permissions.PolicyPushMain},
		{"backquotes", on, shell("echo `" + force + "`"), VerdictAsk, permissions.PolicyPushMain},
		{"program path", on, shell("/usr/bin/git push --force origin main"), VerdictAsk, permissions.PolicyPushMain},
		{"behind a tool's prefix", on, shell("rtk rm -rf /"), VerdictAsk, permissions.PolicyRmRf},
		{"approved part in a compound", on, shell("rm -rf dist && rm -rf src"), VerdictAsk, permissions.PolicyRmRf},
		{"cat a key", on, shell("cat ./certs/server.pem"), VerdictAsk, permissions.PolicyKeys},
		{"cat .env quoted", on, shell(`cat ".e"nv`), VerdictAsk, permissions.PolicyEnv},
		{"a template", on, shell("cat .env.example"), VerdictNone, ""},
		{"a word with .env inside", on, shell("grep process.env src"), VerdictNone, ""},
		{"read pre-approved", on, Call{CallRead, "/repo/certs/server.pem"}, VerdictAllow, permissions.PolicyKeys},
		{"read another key", on, Call{CallRead, "/repo/certs/other.pem"}, VerdictAsk, permissions.PolicyKeys},
		{"read the same key by another spelling", on, Call{CallRead, "/repo/certs/../certs/server.pem"}, VerdictAsk, permissions.PolicyKeys},
		{"write the pre-approved read", on, Call{CallWrite, "/repo/certs/server.pem"}, VerdictAsk, permissions.PolicyKeys},
		{"read .ENV", on, Call{CallRead, "/repo/.ENV"}, VerdictAsk, permissions.PolicyEnv},
		{"read a plain file", on, Call{CallRead, "/repo/main.go"}, VerdictNone, ""},
		{"search for keys", on, Call{CallSearch, "**/*.pem"}, VerdictAsk, permissions.PolicyKeys},
		{"search .env files", on, Call{CallSearch, ".env*"}, VerdictAsk, permissions.PolicyEnv},
		{"a pre-approved command as a search", on, Call{CallSearch, force}, VerdictNone, ""},
		{"headless denies", PolicyPlan{}, shell(force), VerdictDeny, permissions.PolicyPushMain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.plan.DecideCall(tt.call, pre)
			if got.Verdict != tt.want || got.Group != tt.group {
				t.Errorf("DecideCall(%q) = %+v, want %s %s", tt.call.Text, got, tt.want, tt.group)
			}
		})
	}
}

// TestDecideStrict: a strict group's match says so, pre-approved or not.
func TestDecideStrict(t *testing.T) {
	on := PolicyPlan{OnScreen: true, Branches: []string{"trunk2"}}
	for _, tt := range []struct {
		cmd    string
		strict bool
	}{
		{"git push --force origin main", true},
		{"git push -f origin trunk2", true},
		{"git push --force origin feature", false},
		{"rm -rf build", false},
		{"cat id_ed25519", true},
	} {
		if got := on.DecideCall(Call{CallShell, tt.cmd}, PreApproved{}); got.Strict != tt.strict || got.Verdict != VerdictAsk {
			t.Errorf("%q: %+v, want ask, strict %v", tt.cmd, got, tt.strict)
		}
	}
}

// TestDecideCoversStaticRules (T6976): the hook's matcher asks about every
// command a static rule matches, so moving the asks to the hook loses none.
// Each rule's * is filled with text, as a command matching it would hold.
func TestDecideCoversStaticRules(t *testing.T) {
	on := PolicyPlan{OnScreen: true, Branches: []string{"dev"}}
	for _, d := range on.Decide() {
		tool, spec, _, ok := ruleParts(d.Text)
		if !ok {
			t.Fatalf("rule %s", d.Text)
		}
		var c Call
		switch tool {
		case "Bash":
			c = Call{CallShell, strings.ReplaceAll(spec, "*", "x y")}
		case "Read":
			c = Call{CallRead, "/some/dir/" + strings.ReplaceAll(strings.TrimPrefix(spec, "**/"), "*", "a")}
		}
		if got := on.DecideCall(c, PreApproved{}); got.Verdict != VerdictAsk {
			t.Errorf("%s: %q decided %+v, want ask", d.Text, c.Text, got)
		}
		if tool == "Read" {
			name := strings.ReplaceAll(strings.TrimPrefix(spec, "**/"), "*", "a")
			if got := on.DecideCall(Call{CallShell, "cat " + name}, PreApproved{}); got.Verdict != VerdictAsk {
				t.Errorf("%s: cat %s decided %+v, want ask", d.Text, name, got)
			}
		}
	}
}

// TestPreApprovedFrom (T6976): only exact allows from aggy's own file
// pre-approve. A wildcard, a rule from a Claude settings file (the person's or
// the repo's), aggy's config, a CLI's saved approval, or anything while the
// set withholds its allows, never does.
func TestPreApprovedFrom(t *testing.T) {
	rule := func(text string, o permissions.Origin) permissions.Rule {
		return permissions.Rule{Kind: permissions.Allow, Text: text, Origin: o}
	}
	s := permissions.Set{Rules: []permissions.Rule{
		rule("Bash(git push --force origin main)", permissions.FromAggy),
		rule("Bash(rm -rf dist)", permissions.FromAggyRepo),
		rule("Bash(git push *)", permissions.FromAggy),
		rule("Bash(git push:*)", permissions.FromAggy),
		rule("Bash(rm -rf build)", permissions.FromClaudeUser),
		rule("Bash(rm -rf out)", permissions.FromClaudeLocal),
		rule("Bash(rm -rf tmp)", permissions.FromRepoLocal),
		rule("Bash(rm -rf a)", permissions.FromConfig),
		rule("Bash(rm -rf b)", permissions.FromGrokGrant),
		rule("Bash(rm -rf c)", permissions.FromCodexGrant),
		rule("Read(./certs/server.pem)", permissions.FromAggyRepo),
		rule("Read(//etc/ssl/k.pem)", permissions.FromAggy),
		rule("Read(./certs/*.pem)", permissions.FromAggy),
		rule("Read(~/.ssh/id_rsa)", permissions.FromAggy),
		{Kind: permissions.Ask, Text: "Bash(rm -rf x)", Origin: permissions.FromAggy},
	}}
	got := PreApprovedFrom(s, "/repo")
	if strings.Join(got.Commands, "|") != "git push --force origin main|rm -rf dist" {
		t.Errorf("commands = %q", got.Commands)
	}
	if strings.Join(got.Reads, "|") != "/repo/certs/server.pem|/etc/ssl/k.pem" {
		t.Errorf("reads = %q", got.Reads)
	}
	s.Withhold = "a file could not be read"
	if got := PreApprovedFrom(s, "/repo"); len(got.Commands)+len(got.Reads) != 0 {
		t.Errorf("withheld: %+v", got)
	}
}

// TestOpenCodeStrictAsk (T6976): the strict shell rules ask on OpenCode only
// with the evaluate hook, in a trusted repo, without --auto.
func TestOpenCodeStrictAsk(t *testing.T) {
	ds := PolicyPlan{OnScreen: true}.Decide()
	strictIn := func(rules []string) bool {
		for _, r := range rules {
			if strings.Contains(r, "-f*main") || strings.Contains(r, "--force*main") {
				return true
			}
		}
		return false
	}
	for _, tt := range []struct {
		c   OpenCodeConfig
		ask bool
	}{
		{OpenCodeConfig{Trusted: true, Decide: "/p"}, true},
		{OpenCodeConfig{Trusted: false, Decide: "/p"}, false},
		{OpenCodeConfig{Trusted: true, Decide: "/p", Auto: true}, false},
		{OpenCodeConfig{Trusted: true}, false},
	} {
		ask, deny := OpenCodePolicyFor(ds, tt.c.Auto, tt.c.strictAsk())
		if strictIn(ask) != tt.ask || strictIn(deny) == tt.ask {
			t.Errorf("%+v: strict asks %v, denies %v; want asks %v", tt.c, strictIn(ask), strictIn(deny), tt.ask)
		}
	}
}
