//go:build unix

package execrun

import (
	"slices"
	"strings"
	"testing"
)

// TestPTYEnvAntigravity (T6957): an Antigravity launch sets its decision
// plan's two variables, drops inherited copies, and sets no key or base URL;
// the person's own variables, keys among them, are theirs.
func TestPTYEnvAntigravity(t *testing.T) {
	base := []string{"HOME=/h", "PATH=/bin", "GEMINI_API_KEY=k", "AGGY_DECIDE_PLAN=/outer/plan.json", "AGGY_DECIDE_SHA256=00"}
	extra := []string{"AGGY_VENDOR=antigravity", "AGGY_DECIDE_PLAN=/l/policy-decide.json", "AGGY_DECIDE_SHA256=" + strings.Repeat("a", 64)}
	env, err := PTYEnv(base, extra)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range extra {
		if !slices.Contains(env, kv) {
			t.Errorf("%s was not set: %q", kv, env)
		}
	}
	for _, gone := range []string{"AGGY_DECIDE_PLAN=/outer", "AGGY_DECIDE_SHA256=00"} {
		if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, gone) }) {
			t.Errorf("%s survived: %q", gone, env)
		}
	}
	if !slices.Contains(env, "GEMINI_API_KEY=k") {
		t.Error("the person's own variable was dropped")
	}
	// --no-policy: no plan set, and an inherited one does not reach the hook.
	env, err = PTYEnv(base, []string{"AGGY_VENDOR=antigravity"})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(env, func(kv string) bool { return strings.HasPrefix(kv, "AGGY_DECIDE_") }) {
		t.Errorf("an inherited plan reached agy: %q", env)
	}
	for _, bad := range [][]string{
		{"AGGY_VENDOR=antigravity", "GEMINI_API_KEY=k"},
		{"AGGY_VENDOR=antigravity", "GOOGLE_GEMINI_BASE_URL=http://x"},
	} {
		if _, err := PTYEnv(nil, bad); err == nil {
			t.Errorf("%q passed", bad)
		}
	}
}
