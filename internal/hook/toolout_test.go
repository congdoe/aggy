package hook

import (
	"errors"
	"strings"
	"testing"
)

func TestFilterToolOutput(t *testing.T) {
	big := `{"hookSpecificOutput":{"permissionDecisionReason":"` + strings.Repeat("x", MaxToolOutput) + `"}}`
	for _, c := range []struct {
		name, event, in, want string
		err                   bool
	}{
		{"empty", "PreToolUse", " \n", "", false},
		// F-6: rtk hook claude's answer passes as it is.
		{"rewrite", "PreToolUse", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecisionReason":"RTK auto-rewrite","updatedInput":{"command":"rtk git status"}}}`,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecisionReason":"RTK auto-rewrite","updatedInput":{"command":"rtk git status"}}}`, false},
		// S63-9: rtk hook codex's allow is dropped; the rewrite stays.
		{"allow stripped", "PreToolUse", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"x"}}}`,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","updatedInput":{"command":"x"}}}`, false},
		{"allow alone", "PreToolUse", `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow"}}`, "", false},
		{"deny passes", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":"deny","permissionDecisionReason":"no"}}`,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"no"}}`, false},
		{"ask passes", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":"ask"}}`,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"ask"}}`, false},
		{"other keys dropped", "PreToolUse", `{"continue":false,"stopReason":"x","systemMessage":"y","decision":"approve","hookSpecificOutput":{"additionalContext":"z","permissionDecision":"deny"}}`,
			`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"}}`, false},
		{"too big", "PreToolUse", big, "", true},
		{"invalid", "PreToolUse", `{"hookSpecificOutput":`, "", true},
		{"two objects", "PreToolUse", `{} {}`, "", true},
		{"not an object", "PreToolUse", `["allow"]`, "", true},
		{"plain text", "PreToolUse", `approved`, "", true},
		{"decision not a string", "PreToolUse", `{"hookSpecificOutput":{"permissionDecision":1}}`, "", true},
		{"another event's name", "PreToolUse", `{"hookSpecificOutput":{"hookEventName":"PermissionRequest","permissionDecision":"deny"}}`, "", true},
		{"another event", "PostToolUse", `{"hookSpecificOutput":{"additionalContext":"x"}}`, "", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := FilterToolOutput(c.event, []byte(c.in))
			if string(got) != c.want || (err != nil) != c.err || err != nil && !errors.Is(err, ErrToolOutput) {
				t.Errorf("= %s, %v; want %s", got, err, c.want)
			}
		})
	}
}
