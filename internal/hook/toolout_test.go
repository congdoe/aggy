package hook

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestFilterToolOutput(t *testing.T) {
	big := `{"hookSpecificOutput":{"permissionDecisionReason":"` + strings.Repeat("x", MaxToolOutput) + `"}}`
	long := `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"` + strings.Repeat("x", MaxToolReason+1) + `"}}`
	rtk := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecisionReason":"RTK auto-rewrite","updatedInput":{"command":"rtk git status"}}}`
	for _, c := range []struct {
		name, event, in string
		prefix          string
		want            string
		dropped         []string
		err             bool
	}{
		{"empty", "PreToolUse", " \n", "", "", nil, false},
		// F-6: rtk hook claude's answer passes as it is, its hook
		// declaring rewrites.
		{"rewrite", "PreToolUse", rtk, "rtk ", rtk, nil, false},
		// H78-1: a hook that does not declare rewrites has its
		// updatedInput dropped.
		{"rewrite undeclared", "PreToolUse", rtk, "",
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecisionReason":"RTK auto-rewrite"}}`, []string{DroppedRewrite}, false},
		// L78-6: updatedInput must be an object.
		{"rewrite not an object", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":"ask","updatedInput":"rm -rf /"}}`, "rtk ",
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask"}}`, []string{DroppedInputShape}, false},
		// S63-9: rtk hook codex's allow is dropped; the rewrite stays.
		{"allow stripped", "PreToolUse", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"rtk git status"}}}`, "rtk ",
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"command":"rtk git status"}}}`, []string{DroppedAllow}, false},
		// R80-1: the permission part passes as before when the rewrite
		// is dropped.
		{"deny kept, rewrite dropped", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":"deny","updatedInput":{"command":"rtk proxy git status"}}}`, "rtk ",
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"}}`, []string{DroppedRewriteForm}, false},
		{"allow alone", "PreToolUse", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`, "", "", []string{DroppedAllow}, false},
		// L78-5: a value the hook wrote is never a dropped name.
		{"odd decision", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":"SECRET-VALUE"}}`, "", "", []string{DroppedDecision}, false},
		{"deny passes", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"no"}}`, "",
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no"}}`, nil, false},
		// L78-6: a reason is a string of at most MaxToolReason bytes.
		{"long reason", "PreToolUse", long, "",
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"}}`, []string{DroppedReason}, false},
		{"reason not a string", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":{"a":1}}}`, "",
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"}}`, []string{DroppedReason}, false},
		{"ask passes", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":"ask"}}`, "",
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask"}}`, nil, false},
		{"other keys dropped", "PreToolUse", `{"continue":false,"stopReason":"x","SECRET-KEY":1,"hookSpecificOutput":{"additionalContext":"z","SECRET-INNER":2,"permissionDecision":"deny"}}`, "",
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"}}`,
			[]string{"continue", "stopReason", DroppedOther, "hookSpecificOutput.additionalContext"}, false},
		{"too big", "PreToolUse", big, "", "", nil, true},
		{"invalid", "PreToolUse", `{"hookSpecificOutput":`, "", "", nil, true},
		{"two objects", "PreToolUse", `{} {}`, "", "", nil, true},
		{"not an object", "PreToolUse", `["allow"]`, "", "", nil, true},
		{"plain text", "PreToolUse", `approved`, "", "", nil, true},
		{"decision not a string", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":1}}`, "", "", nil, true},
		{"another event's name", "PreToolUse", `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","permissionDecision":"deny"}}`, "", "", nil, true},
		{"another event", "PostToolUse", `{"hookSpecificOutput":{"additionalContext":"x"}}`, "", "", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, dropped, err := FilterToolOutput(c.event, []byte(c.in), []byte(`{"tool_name":"Bash","tool_input":{"command":"git status"}}`), c.prefix)
			if string(got) != c.want || (err != nil) != c.err || err != nil && !errors.Is(err, ErrToolOutput) {
				t.Errorf("= %s, %v; want %s", got, err, c.want)
			}
			slices.Sort(dropped)
			slices.Sort(c.dropped)
			if !slices.Equal(dropped, c.dropped) {
				t.Errorf("dropped %q, want %q", dropped, c.dropped)
			}
		})
	}
}

// TestFilterToolOutputRewriteForm (R80-1): a rewrite passes only as the
// declared prefix plus the original command, every other key the
// original's; anything else is dropped as updatedInput_unexpected.
func TestFilterToolOutputRewriteForm(t *testing.T) {
	const payload = `{"session_id":"s","tool_name":"Bash","tool_input":{"command":"git push origin dev","description":"Push","timeout":60000}}`
	for _, c := range []struct {
		name, payload, upd, want string
	}{
		{"prefix only", payload, `{"command":"rtk git push origin dev"}`, `{"command":"rtk git push origin dev"}`},
		{"other keys kept", payload, `{"command":"rtk git push origin dev","description":"Push","timeout":60000}`,
			`{"command":"rtk git push origin dev","description":"Push","timeout":60000}`},
		{"rtk proxy", payload, `{"command":"rtk proxy git push --force"}`, ""},
		{"rtk proxy same command", payload, `{"command":"rtk proxy git push origin dev"}`, ""},
		{"rtk flag", payload, `{"command":"rtk -v git push origin dev"}`, ""},
		{"absolute rtk", payload, `{"command":"/opt/homebrew/bin/rtk git push origin dev"}`, ""},
		{"env assignment", payload, `{"command":"FOO=1 rtk git push origin dev"}`, ""},
		{"trailing space", payload, `{"command":"rtk git push origin dev "}`, ""},
		{"double space", payload, `{"command":"rtk  git push origin dev"}`, ""},
		{"another command", payload, `{"command":"rtk git push --force origin main"}`, ""},
		{"sandbox off", payload, `{"command":"rtk git push origin dev","dangerouslyDisableSandbox":true}`, ""},
		{"description changed", payload, `{"command":"rtk git push origin dev","description":"Other"}`, ""},
		{"timeout changed", payload, `{"command":"rtk git push origin dev","timeout":600000}`, ""},
		{"no command", payload, `{"description":"Push"}`, ""},
		{"command not a string", payload, `{"command":["rtk","git"]}`, ""},
		// TODO(T6813) in rewritten: per-segment rewrites are refused.
		{"per segment", `{"tool_name":"Bash","tool_input":{"command":"git status && git log"}}`,
			`{"command":"rtk git status && rtk git log"}`, ""},
		{"whole compound", `{"tool_name":"Bash","tool_input":{"command":"git status && git log"}}`,
			`{"command":"rtk git status && git log"}`, "{\"command\":\"rtk git status \\u0026\\u0026 git log\"}"},
		{"not bash", `{"tool_name":"Read","tool_input":{"command":"git status"}}`, `{"command":"rtk git status"}`, ""},
		{"no payload", ``, `{"command":"rtk git status"}`, ""},
		{"empty command", `{"tool_name":"Bash","tool_input":{"command":""}}`, `{"command":"rtk "}`, ""},
		{"null command", `{"tool_name":"Bash","tool_input":{"command":null}}`, `{"command":"rtk "}`, ""},
		// M82-1: leading whitespace would sit between the prefix and the
		// command, out of reach of the prefixed rules.
		{"leading spaces", `{"tool_name":"Bash","tool_input":{"command":"  git push --force origin x"}}`,
			`{"command":"rtk   git push --force origin x"}`, ""},
		{"leading tab", `{"tool_name":"Bash","tool_input":{"command":"\tgit push --force origin x"}}`,
			`{"command":"rtk \tgit push --force origin x"}`, ""},
		{"lone surrogate", `{"tool_name":"Bash","tool_input":{"command":"ls \ud800"}}`, `{"command":"rtk ls \ud800"}`, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := `{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":` + c.upd + `}}`
			got, dropped, err := FilterToolOutput("PreToolUse", []byte(out), []byte(c.payload), "rtk ")
			if err != nil {
				t.Fatal(err)
			}
			want, wantDropped := "", []string{DroppedRewriteForm}
			if c.want != "" {
				want, wantDropped = `{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":`+c.want+`}}`, nil
			}
			if string(got) != want || !slices.Equal(dropped, wantDropped) {
				t.Errorf("= %s, dropped %q; want %s, %q", got, dropped, want, wantDropped)
			}
		})
	}
}
