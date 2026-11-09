package skills

import (
	"strings"
	"testing"
)

// maxCommandAlways caps a command skill's frontmatter (T6910). Claude and Grok
// keep a skill with disable-model-invocation out of the model's context
// (docs/verification.md, Skill folders), but Codex has no such key, so there
// the frontmatter is paid on every turn.
const maxCommandAlways = 80

// TestOwnSkills: each of aggy's skills names its folder, has a description, and
// stays small; each command skill is the person's alone, not the model's.
func TestOwnSkills(t *testing.T) {
	seen := map[string]bool{}
	for _, sk := range Own() {
		name, desc := Front(sk.Content)
		if name != sk.Name || desc == "" || seen[name] {
			t.Errorf("%s: frontmatter name %q, description %q", sk.Name, name, desc)
		}
		seen[name] = true
		if sk.Name == "aggy" {
			continue
		}
		front, _ := splitFrontmatter(sk.Content)
		if !strings.Contains(string(front), "\ndisable-model-invocation: true\n") {
			t.Errorf("%s: not marked disable-model-invocation", sk.Name)
		}
		always, body := Estimate(sk.Content)
		if always > maxCommandAlways || body == 0 || body > maxBody {
			t.Errorf("%s: about %d tokens always, %d on invoke", sk.Name, always, body)
		}
	}
	for _, want := range []string{"aggy", "switch", "borrow", "inbox"} {
		if !seen[want] {
			t.Errorf("no %s skill", want)
		}
	}
}
