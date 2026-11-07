package claude

import (
	"os"
	"path/filepath"
)

// SelfTestInputs is the user's settings.json: a review runs with
// --setting-sources user, so its permission rules apply to the self-test.
func (Adapter) SelfTestInputs() []string {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return nil
		}
		dir = filepath.Join(home, ".claude")
	}
	return []string{filepath.Join(dir, "settings.json")}
}

// SelfTestHint names what in those settings can allow the read. Claude's docs
// say reads need no approval only within the working directory and additional
// directories (https://code.claude.com/docs/en/permissions.md), so such a rule
// should do it. Unverified live (docs/verification.md, T6929).
func (Adapter) SelfTestHint() string {
	return "a review loads your own Claude settings, so this may be your rule and not the new version: a Read allow rule or permissions.additionalDirectories in settings.json that reaches outside the working directory does it; change it and the next borrow tests again"
}
