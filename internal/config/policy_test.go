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
}

// TestRepoPolicyIgnored: a repo's [policy] table, whatever it holds, is read
// only so the launch can say it is ignored (T6979): it relaxes nothing and
// turns no group off.
func TestRepoPolicyIgnored(t *testing.T) {
	cases := []struct {
		name, text string
		sets       bool
	}{
		{"none", "tools_off = []\n", false},
		{"required off", "[policy]\nrequired = false\n", true},
		{"a group off", "[policy]\nkey-read = \"allow\"\n", true},
		{"empty table", "[policy]\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rc, err := ParseRepo(c.text)
			if err != nil {
				t.Fatalf("ParseRepo: %v", err)
			}
			if rc.SetsPolicy() != c.sets {
				t.Errorf("SetsPolicy = %v, want %v", rc.SetsPolicy(), c.sets)
			}
			e := Default()
			if !e.Policy.Required {
				t.Error("a repo config relaxed the safety policy")
			}
		})
	}
}
