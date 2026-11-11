package codex

import "strings"

// loosenEffect is PassFlags.Effect (T6979): what a loosening flag does to
// aggy's safety policy on Codex, said in the question aggy asks before letting
// it through.
func loosenEffect(flag string) string {
	switch {
	case flag == "--ask-for-approval never" || flag == "-a never":
		// prompt_is_rejected_by_policy, core/src/exec_policy.rs at rust-v0.164.0
		// (the same source launchOnce cites for a borrow child reopened).
		return "Codex then refuses what aggy's rules file would ask, with no prompt, and runs every other command without asking (core/src/exec_policy.rs at rust-v0.164.0)"
	case strings.HasPrefix(flag, "--sandbox ") || strings.HasPrefix(flag, "-s "):
		return "it takes Codex's sandbox away or changes it; aggy's rules file still asks as before, since it does not change approvals (unverified live)"
	case flag == "--yolo" || flag == "--approve-for-me" || flag == "--not-so-yolo" || strings.Contains(flag, "dangerously"):
		return "it skips Codex's approvals and its sandbox; whether Codex still applies aggy's rules file then is unverified, so treat the policy as off"
	case flag == "--ignore-user-config":
		return "Codex then skips your config.toml, aggy's hooks wiring with it; aggy has not checked whether its rules file still loads, so treat the policy as off"
	}
	return ""
}
