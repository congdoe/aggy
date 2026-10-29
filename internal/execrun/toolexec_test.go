package execrun

import (
	"slices"
	"testing"
)

// TestToolEnv: W71-2, a tool server's environment is the basics and its
// declared variables, nothing else from the CLI's.
func TestToolEnv(t *testing.T) {
	base := []string{"HOME=/h", "PATH=/bin:.:rel:/usr/bin", "DEMO_HOME=/d", "DEMO_X=1", "OPENAI_API_KEY=sk",
		"NODE_OPTIONS=--require x", "npm_config_registry=evil", "AGGY_HOME=/s", "CODEX_HOME=/c", "LANG=C", "BAD"}
	got, err := ToolEnv(base, []string{"DEMO_HOME", "DEMO_UNSET"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"HOME=/h", "PATH=/bin:/usr/bin", "DEMO_HOME=/d", "LANG=C"}; !slices.Equal(got, want) {
		t.Errorf("env = %q, want %q", got, want)
	}
	for _, n := range []string{"PATH", "HOME", "AGGY_HOME", "A=B", ""} {
		if _, err := ToolEnv(base, []string{n}); err == nil {
			t.Errorf("%q passed", n)
		}
	}
}
