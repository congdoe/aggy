package codex

import (
	"slices"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// TestNewFeatures reads codex features list as cli/src/main.rs#L1951 at
// 6aaac69 prints it: a stage may be two words, and a key may hold a dot.
func TestNewFeatures(t *testing.T) {
	base := "apps  stable  true\ngoals  stable  true\nimage_generation  stable  true\nplugins  stable  true\nshell_tool  stable  true\nsleep_tool  stable  true\nunified_exec  stable  true\nview_image  stable  true\n"
	tests := []struct {
		name    string
		out     adapter.Output
		want    []string
		wantErr string
	}{
		{name: "known", out: adapter.Output{Stdout: []byte(base + "multi_agent  stable  true\n")}},
		{name: "two-word stage, dotted key, off", out: adapter.Output{Stdout: []byte(base + "guardianv2.thread_context  under development  false\n")}},
		{name: "new and on", out: adapter.Output{Stdout: []byte(base + "zeta  under development  true\nalpha  stable  true\n")}, want: []string{"alpha", "zeta"}},
		{name: "off by default at the tag, now on", out: adapter.Output{Stdout: []byte(base + "memories  stable  true\n")}, want: []string{"memories"}},
		{name: "empty", out: adapter.Output{}, wantErr: "no features"},
		{name: "a row it cannot read", out: adapter.Output{Stdout: []byte(base + "Warning: something\n")}, wantErr: "cannot read"},
		{name: "not a boolean", out: adapter.Output{Stdout: []byte(base + "x  stable  maybe\n")}, wantErr: "cannot read"},
		{name: "a lockdown key gone", out: adapter.Output{Stdout: []byte(strings.Replace(base, "sleep_tool  stable  true\n", "", 1))}, wantErr: "no longer names sleep_tool"},
		{name: "failed", out: adapter.Output{ExitCode: 3}, wantErr: "exited 3"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Adapter{}.NewFeatures(tt.out)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || !slices.Equal(got, tt.want) {
				t.Errorf("got %v, %v, want %v", got, err, tt.want)
			}
		})
	}
}

// TestFeaturesOnCoverLockdown: every feature the lockdown turns off is one
// reviewed as on, so a clean list at the tested version passes.
func TestFeaturesOnCoverLockdown(t *testing.T) {
	for i, a := range Lockdown {
		if a == "--disable" && !slices.Contains(featuresOn, Lockdown[i+1]) {
			t.Errorf("%s is not in featuresOn", Lockdown[i+1])
		}
	}
	if !slices.IsSorted(featuresOn) {
		t.Error("featuresOn is not sorted")
	}
}
