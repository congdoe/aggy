package antigravity

import "github.com/congdoe/aggy/internal/adapter"

// loosening are agy's flags that skip or loosen its own approvals (LIVE
// help, 1.5.3): asked in place one by one, or let through with --no-policy.
// --mode and --add-dir are Checked, by value.
var loosening = []adapter.RefusedFlag{
	{Names: []string{"--dangerously-skip-permissions"}, Why: "it skips agy's own approval prompt", UnlessNoPolicy: true},
}

// loosenEffect is PassFlags.Effect (T6979): what a loosening flag does to
// aggy's safety policy on Antigravity, said in the question aggy asks before
// letting it through for one launch.
func loosenEffect(flag string) string {
	switch flag {
	case "--dangerously-skip-permissions":
		// Whether agy still runs a PreToolUse hook's force_ask in that mode is
		// unverified.
		return "agy then runs tools without its own approval prompt; aggy's hook still refuses what the policy denies, but whether its asks still reach you is unchecked, so treat them as off"
	case "--mode auto", "--mode accept-edits":
		return "agy then asks you less on its own; aggy's hook still refuses what the policy denies and asks with force_ask, which agy showed even over an allow rule, but not in this mode, which is unchecked"
	case "--add-dir":
		return "agy's workspace then reaches that folder too; aggy's hook judges calls there as anywhere, and a folder of keys or config is refused unless --no-policy"
	}
	return ""
}
