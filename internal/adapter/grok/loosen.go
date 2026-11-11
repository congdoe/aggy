package grok

// loosenEffect is PassFlags.Effect (T6979): what a loosening flag does to
// aggy's safety policy on Grok Build, said in the question aggy asks before
// letting it through.
func loosenEffect(flag string) string {
	switch flag {
	case "--always-approve", "--yolo", "--dangerously-skip-permissions":
		return "Grok then approves every call without asking; aggy has not checked whether its --deny rules still hold, so treat the policy as off"
	case "--trust", "--trust-folder":
		return "it trusts the folder for Grok, which then loads the repo's own settings and hooks, which aggy cannot filter; aggy's --deny rules stay"
	}
	return ""
}
