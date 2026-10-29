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

// TestSetEnv (T6822): an opt-out aggy sets replaces the person's own
// value; it may not name a variable the environment is built from, or
// one of aggy's.
func TestSetEnv(t *testing.T) {
	env := []string{"HOME=/h", "DO_NOT_TRACK=0", "DEMO_T=1", "LANG=C"}
	got, err := SetEnv(env, []string{DoNotTrack, "DEMO_T=0"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"HOME=/h", "LANG=C", "DO_NOT_TRACK=1", "DEMO_T=0"}; !slices.Equal(got, want) {
		t.Errorf("env = %q, want %q", got, want)
	}
	if got, _ := SetEnv(env, nil); !slices.Equal(got, env) {
		t.Errorf("nothing set: %q", got)
	}
	for _, kv := range []string{"PATH=/x", "HOME=/x", "AGGY_HOME=/x", "NOVALUE", "=1"} {
		if _, err := SetEnv(env, []string{kv}); err == nil {
			t.Errorf("%q set", kv)
		}
	}
}

// TestProxyEnv (T6822, MED-2 of #97): a proxy's environment is
// ToolEnv's with the telemetry part: DO_NOT_TRACK=1 over the person's
// own by default, and the person's own passed when telemetry is allowed.
func TestProxyEnv(t *testing.T) {
	base := []string{"HOME=/h", "DO_NOT_TRACK=0", "AGGY_SOCKET=/s", "ANTHROPIC_API_KEY=k"}
	if got, err := ProxyEnv(base, nil, []string{DoNotTrack}); err != nil || !slices.Equal(got, []string{"HOME=/h", "DO_NOT_TRACK=1"}) {
		t.Errorf("default: %q %v", got, err)
	}
	if got, err := ProxyEnv(base, []string{"DO_NOT_TRACK"}, nil); err != nil || !slices.Equal(got, []string{"HOME=/h", "DO_NOT_TRACK=0"}) {
		t.Errorf("allowed: %q %v", got, err)
	}
	if _, err := ProxyEnv(base, []string{"AGGY_SOCKET"}, nil); err == nil {
		t.Error("an aggy variable passed")
	}
}
