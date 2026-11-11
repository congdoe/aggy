package hermes

// loosenEffect is PassFlags.Effect (T6979): what a loosening flag does to
// aggy's safety policy on Hermes, said in the question aggy asks before
// letting it through for one launch. From hermes_cli/_parser.py:193-212 at
// v0.23.1 and the code each flag reaches.
func loosenEffect(flag string) string {
	switch flag {
	case "--yolo":
		// tools/approval.py:1189-1212: approvals.deny and the hardline floor
		// run before the yolo bypass.
		return "Hermes then runs every terminal command without asking; aggy's denies in approvals.deny and its hook's refusals still hold, but whether its hook's asks still reach you is unchecked"
	case "--accept-hooks":
		// agent/shell_hooks.py:816-823.
		return "Hermes then registers every configured hook without asking you, in aggy's profile for this repo; aggy's own hooks are unchanged"
	case "--ignore-user-config":
		// _parser.py:206-207; hermes_cli/main.py:1599-1601 runs it outside the
		// gateway-backed CLI, where aggy's hooks have not been checked.
		return "Hermes then ignores the profile config that holds aggy's approvals, denies, and hooks, so treat the policy as off"
	case "--safe-mode":
		// _parser.py:210-211: implies --ignore-user-config and --ignore-rules.
		return "Hermes then ignores the profile config that holds aggy's approvals, denies, and hooks, and skips memory and context files, so treat the policy as off"
	case "--ignore-rules":
		// _parser.py:208-209; whether it drops aggy's ephemeral memory
		// variable too is unchecked.
		return "Hermes then skips its context files and memory, possibly aggy's shared memory too (unchecked); the policy itself is unchanged"
	}
	return ""
}
