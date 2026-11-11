package claude

import "strings"

// loosenEffect is PassFlags.Effect (T6979): what a loosening flag does to
// aggy's safety policy on Claude Code, said in the question aggy asks before
// letting it through.
func loosenEffect(flag string) string {
	mode, isMode := strings.CutPrefix(flag, "--permission-mode ")
	switch {
	case flag == "--dangerously-skip-permissions" || flag == "--allow-dangerously-skip-permissions" || isMode && mode == "bypassPermissions":
		// noPromptModes (permission-modes.md, read 2026-11-10): Claude shows
		// no prompt, and aggy's decision hook refuses what it would ask.
		return "Claude asks nothing in this mode (permission-modes.md), so aggy's safety policy cannot ask: where aggy's decision hook runs it refuses what the policy would ask, and every other call runs without asking. aggy has not checked how Claude's static rules behave in it"
	case isMode:
		return "aggy does not know this permission mode, so treat its safety policy as off for what the mode allows"
	}
	return ""
}
