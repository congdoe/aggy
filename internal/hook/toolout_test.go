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
		rewrites        bool
		want            string
		dropped         []string
		err             bool
	}{
		{"empty", "PreToolUse", " \n", false, "", nil, false},
		// F-6: rtk hook claude's answer passes as it is, its hook
		// declaring rewrites.
		{"rewrite", "PreToolUse", rtk, true, rtk, nil, false},
		// H78-1: a hook that does not declare rewrites has its
		// updatedInput dropped.
		{"rewrite undeclared", "PreToolUse", rtk, false,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecisionReason":"RTK auto-rewrite"}}`, []string{DroppedRewrite}, false},
		// L78-6: updatedInput must be an object.
		{"rewrite not an object", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":"ask","updatedInput":"rm -rf /"}}`, true,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask"}}`, []string{DroppedInputShape}, false},
		// S63-9: rtk hook codex's allow is dropped; the rewrite stays.
		{"allow stripped", "PreToolUse", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"x"}}}`, true,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"command":"x"}}}`, []string{DroppedAllow}, false},
		{"allow alone", "PreToolUse", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`, false, "", []string{DroppedAllow}, false},
		// L78-5: a value the hook wrote is never a dropped name.
		{"odd decision", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":"SECRET-VALUE"}}`, false, "", []string{DroppedDecision}, false},
		{"deny passes", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"no"}}`, false,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no"}}`, nil, false},
		// L78-6: a reason is a string of at most MaxToolReason bytes.
		{"long reason", "PreToolUse", long, false,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"}}`, []string{DroppedReason}, false},
		{"reason not a string", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":{"a":1}}}`, false,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"}}`, []string{DroppedReason}, false},
		{"ask passes", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":"ask"}}`, false,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask"}}`, nil, false},
		{"other keys dropped", "PreToolUse", `{"continue":false,"stopReason":"x","SECRET-KEY":1,"hookSpecificOutput":{"additionalContext":"z","SECRET-INNER":2,"permissionDecision":"deny"}}`, false,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"}}`,
			[]string{"continue", "stopReason", DroppedOther, "hookSpecificOutput.additionalContext"}, false},
		{"too big", "PreToolUse", big, false, "", nil, true},
		{"invalid", "PreToolUse", `{"hookSpecificOutput":`, false, "", nil, true},
		{"two objects", "PreToolUse", `{} {}`, false, "", nil, true},
		{"not an object", "PreToolUse", `["allow"]`, false, "", nil, true},
		{"plain text", "PreToolUse", `approved`, false, "", nil, true},
		{"decision not a string", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":1}}`, false, "", nil, true},
		{"another event's name", "PreToolUse", `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","permissionDecision":"deny"}}`, false, "", nil, true},
		{"another event", "PostToolUse", `{"hookSpecificOutput":{"additionalContext":"x"}}`, false, "", nil, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, dropped, err := FilterToolOutput(c.event, []byte(c.in), c.rewrites)
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
