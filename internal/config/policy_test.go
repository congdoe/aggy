package config

import "testing"

// TestPolicyRequired: the safety policy is required by default; only the
// user's own config can relax it, and a repo's config cannot.
func TestPolicyRequired(t *testing.T) {
	if !Default().Policy.Required {
		t.Error("the policy is not required by default")
	}
	c, err := Parse("[policy]\nrequired = false\n")
	if err != nil || c.Policy.Required {
		t.Errorf("user opt-out: %+v, %v", c.Policy, err)
	}
	if _, err := ParseRepo("[policy]\nrequired = false\n"); err == nil {
		t.Error("a repo config relaxed the safety policy")
	}
}
