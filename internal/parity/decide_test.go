package parity

import (
	"encoding/json"
	"slices"
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
	pre := PreApproved{Commands: []string{force, "rm -rf dist"}, Reads: []ReadApproval{{Path: "/repo/certs/server.pem"}},
		CheckRead: func(ReadApproval) bool { return true }}
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
		{"read pre-approved", on, Call{Kind: CallRead, Text: "/repo/certs/server.pem"}, VerdictAllow, permissions.PolicyKeys},
		{"read another key", on, Call{Kind: CallRead, Text: "/repo/certs/other.pem"}, VerdictAsk, permissions.PolicyKeys},
		{"read the same key by another spelling", on, Call{Kind: CallRead, Text: "/repo/certs/../certs/server.pem"}, VerdictAsk, permissions.PolicyKeys},
		{"write the pre-approved read", on, Call{Kind: CallWrite, Text: "/repo/certs/server.pem"}, VerdictAsk, permissions.PolicyKeys},
		{"read .ENV", on, Call{Kind: CallRead, Text: "/repo/.ENV"}, VerdictAsk, permissions.PolicyEnv},
		{"read a plain file", on, Call{Kind: CallRead, Text: "/repo/main.go"}, VerdictNone, ""},
		{"search for keys", on, Call{Kind: CallSearch, Text: "**/*.pem"}, VerdictAsk, permissions.PolicyKeys},
		{"search .env files", on, Call{Kind: CallSearch, Text: ".env*"}, VerdictAsk, permissions.PolicyEnv},
		{"a pre-approved command as a search", on, Call{Kind: CallSearch, Text: force}, VerdictNone, ""},
		{"headless denies", PolicyPlan{}, shell(force), VerdictDeny, permissions.PolicyPushMain},
		// PR #129 re-review: another tool's text is never allowed.
		{"another tool, pre-approved", on, Call{Kind: CallShell, Text: force, Other: true}, VerdictNone, permissions.PolicyPushMain},
		{"another tool, not pre-approved", on, Call{Kind: CallShell, Text: "rm -rf ~", Other: true}, VerdictAsk, permissions.PolicyRmRf},
		{"another tool, group denied", denyForce, Call{Kind: CallShell, Text: force, Other: true}, VerdictDeny, permissions.PolicyForce},
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
		if got := on.DecideCall(Call{Kind: CallShell, Text: tt.cmd}, PreApproved{}); got.Strict != tt.strict || got.Verdict != VerdictAsk {
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
			c = Call{Kind: CallShell, Text: strings.ReplaceAll(spec, "*", "x y")}
		case "Read":
			c = Call{Kind: CallRead, Text: "/some/dir/" + strings.ReplaceAll(strings.TrimPrefix(spec, "**/"), "*", "a")}
		}
		if got := on.DecideCall(c, PreApproved{}); got.Verdict != VerdictAsk {
			t.Errorf("%s: %q decided %+v, want ask", d.Text, c.Text, got)
		}
		if tool == "Read" {
			name := strings.ReplaceAll(strings.TrimPrefix(spec, "**/"), "*", "a")
			if got := on.DecideCall(Call{Kind: CallShell, Text: "cat " + name}, PreApproved{}); got.Verdict != VerdictAsk {
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
	ids := func(p string) (uint64, uint64, bool) { return 1, uint64(len(p)), p != "/etc/ssl/k.pem" }
	paths := func(rs []ReadApproval) string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Path)
		}
		return strings.Join(out, "|")
	}
	got := PreApprovedFrom(s, "/repo", ids)
	if strings.Join(got.Commands, "|") != "git push --force origin main|rm -rf dist" {
		t.Errorf("commands = %q", got.Commands)
	}
	// PR #129 review: a read pre-approves only in a trusted repo, and only
	// with a recorded identity (/etc/ssl/k.pem has none here).
	if len(got.Reads) != 0 {
		t.Errorf("untrusted reads = %+v", got.Reads)
	}
	s.Trusted = true
	got = PreApprovedFrom(s, "/repo", ids)
	if paths(got.Reads) != "/repo/certs/server.pem" || got.Reads[0].Ino != uint64(len("/repo/certs/server.pem")) {
		t.Errorf("reads = %+v", got.Reads)
	}
	if got := PreApprovedFrom(s, "/repo", nil); len(got.Reads) != 0 {
		t.Errorf("no identities: reads = %+v", got.Reads)
	}
	if got := strings.Join(ExactReads(s, "/repo"), "|"); got != "/repo/certs/server.pem|/etc/ssl/k.pem" {
		t.Errorf("ExactReads = %q", got)
	}
	s.Withhold = "a file could not be read"
	if got := PreApprovedFrom(s, "/repo", ids); len(got.Commands)+len(got.Reads) != 0 {
		t.Errorf("withheld: %+v", got)
	}
}

// TestDecideReadIdentity (PR #129 review, MEDIUM-4): a read pre-approval
// allows only when CheckRead confirms the file, and never without CheckRead.
func TestDecideReadIdentity(t *testing.T) {
	on := PolicyPlan{OnScreen: true}
	a := ReadApproval{Path: "/repo/certs/server.pem", Dev: 1, Ino: 2}
	c := Call{Kind: CallRead, Text: a.Path}
	for _, tt := range []struct {
		name  string
		check func(ReadApproval) bool
		want  Verdict
	}{
		{"confirmed", func(ReadApproval) bool { return true }, VerdictAllow},
		{"replaced or a link", func(ReadApproval) bool { return false }, VerdictAsk},
		{"no check", nil, VerdictAsk},
	} {
		if got := on.DecideCall(c, PreApproved{Reads: []ReadApproval{a}, CheckRead: tt.check}); got.Verdict != tt.want {
			t.Errorf("%s: %+v, want %s", tt.name, got, tt.want)
		}
	}
}

// TestDecideTooLong (PR #129 review, LOW-2): a call past MaxCallText asks at
// once, even when it is exactly pre-approved.
func TestDecideTooLong(t *testing.T) {
	long := "echo " + strings.Repeat("a", MaxCallText)
	got := PolicyPlan{OnScreen: true}.DecideCall(Call{Kind: CallShell, Text: long}, PreApproved{Commands: []string{long}})
	if got.Verdict != VerdictAsk || got.Reason != ReasonTooLong {
		t.Errorf("long call: %+v", got)
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
		{OpenCodeConfig{Trusted: true, Decide: "/p", DecideSum: "ab"}, true},
		{OpenCodeConfig{Trusted: true, Decide: "/p"}, false},
		{OpenCodeConfig{Trusted: false, Decide: "/p", DecideSum: "ab"}, false},
		{OpenCodeConfig{Trusted: true, Decide: "/p", DecideSum: "ab", Auto: true}, false},
		{OpenCodeConfig{Trusted: true}, false},
	} {
		ask, deny := OpenCodePolicyFor(ds, tt.c.Auto, tt.c.strictAsk())
		if strictIn(ask) != tt.ask || strictIn(deny) == tt.ask {
			t.Errorf("%+v: strict asks %v, denies %v; want asks %v", tt.c, strictIn(ask), strictIn(deny), tt.ask)
		}
	}
}

// TestOpenCodeKeyReadsAsk (owner decision, 2026-11-10): on a trusted launch
// without --auto, the read tool asks on a private key's name rather than being
// refused, and the plugin is told to guard strict matches; untrusted, --auto,
// or the key group set to deny keeps the refusal. .env reads stay refused.
func TestOpenCodeKeyReadsAsk(t *testing.T) {
	base := OpenCodeConfig{Trusted: true, Decide: "/p", DecideSum: "ab", Providers: []string{"openai"}, Plugin: "/plug", Aggy: "/bin/aggy", Root: "/repo",
		Policy: PolicyPlan{OnScreen: true}.Decide()}
	keysDeny := base
	keysDeny.Policy = PolicyPlan{OnScreen: true, Settings: map[string]permissions.Kind{permissions.PolicyKeys: permissions.Deny}}.Decide()
	untrusted, auto := base, base
	untrusted.Trusted, auto.Auto = false, true
	for _, tt := range []struct {
		name string
		c    OpenCodeConfig
		ask  bool
	}{
		{"trusted", base, true},
		{"untrusted", untrusted, false},
		{"auto", auto, false},
		{"key group denied", keysDeny, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b, err := tt.c.Render()
			if err != nil {
				t.Fatal(err)
			}
			var doc openCodeDoc
			if err := json.Unmarshal(b, &doc); err != nil {
				t.Fatal(err)
			}
			has := func(rs []openCodeRule, want openCodeRule) bool { return slices.Contains(rs, want) }
			if got := has(doc.Permissions, openCodeRule{"read", "*/id_ed25519", "ask"}); got != tt.ask {
				t.Errorf("read ask on a key = %v, want %v", got, tt.ask)
			}
			if got := has(doc.Policies, openCodeRule{"tool.use", "read:*/id_ed25519", "deny"}); got == tt.ask {
				t.Errorf("read deny on a key = %v, want %v", got, !tt.ask)
			}
			if !has(doc.Policies, openCodeRule{"tool.use", "read:*/.env", "deny"}) || !has(doc.Policies, openCodeRule{"tool.use", "edit:*/id_ed25519", "deny"}) {
				t.Error(".env reads or key edits are no longer refused")
			}
			if strict := doc.Plugins[0].Options["strict"] == "1"; strict != (tt.name != "untrusted" && tt.name != "auto") {
				t.Errorf("strict option = %v", strict)
			}
		})
	}
}
