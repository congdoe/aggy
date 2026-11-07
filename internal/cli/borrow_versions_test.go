package cli

import (
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/fakecli"
)

// TestBorrowNewVersion runs aggy borrow against the fake at a version above
// the tested range, through the real probe: the one line, the report's words,
// and a help missing a lockdown flag.
func TestBorrowNewVersion(t *testing.T) {
	tests := []struct {
		name       string
		scenario   string
		args       []string
		wantCode   int
		wantStdout []string
		wantStderr []string
	}{
		{
			name:       "checks pass",
			scenario:   `{"version":"1.0.99","json":{"text":"NO ACCESS"}}`,
			args:       []string{"news", "what shipped?"},
			wantStdout: []string{"grok 1.0.99: checked here, not verified by a release", "NO ACCESS"},
			wantStderr: []string{"grok 1.0.99 is newer than aggy's tested versions: checked here (help, a child reported it could not read a planted file)"},
		},
		{
			name:       "help lacks a flag",
			scenario:   `{"version":"1.0.99","help":"Usage: grok\n  --prompt-file <p>\n  --output-format <f>\n  --tools <t>\n  --sandbox <s>\n","json":{"text":"Summary"}}`,
			args:       []string{"news", "what shipped?", "--vendor", "grok"},
			wantCode:   1,
			wantStderr: []string{"does not list what a news lockdown needs", "no-subagents", "--allow-untested"},
		},
		{
			name:       "a new major version",
			scenario:   `{"version":"2.0.0","json":{"text":"Summary"}}`,
			args:       []string{"news", "what shipped?", "--vendor", "grok"},
			wantCode:   1,
			wantStderr: []string{"outside the versions aggy was tested on", "--allow-untested"},
		},
		{
			name:       "a new major version with --allow-untested",
			scenario:   `{"version":"2.0.0","json":{"text":"Summary"}}`,
			args:       []string{"news", "what shipped?", "--vendor", "grok", "--allow-untested"},
			wantStdout: []string{"grok 2.0.0: outside the tested versions, run with --allow-untested, not checked", "Summary"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newBorrowFixture(t, adapter.Grok)
			f.scenario["grok"] = tt.scenario
			code, out, errOut := f.run(t, tt.args...)
			if code != tt.wantCode {
				t.Fatalf("code %d, stderr %q, stdout %q", code, errOut, out)
			}
			for _, w := range tt.wantStdout {
				if !strings.Contains(out, w) {
					t.Errorf("stdout lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tt.wantStderr {
				if !strings.Contains(errOut, w) {
					t.Errorf("stderr lacks %q:\n%s", w, errOut)
				}
			}
		})
	}
}

// TestBorrowSelfTestLimit: a limit a self-test ran into reaches the router, so
// the next borrow skips the vendor and starts nothing.
func TestBorrowSelfTestLimit(t *testing.T) {
	f := newBorrowFixture(t, adapter.Grok)
	f.scenario["grok"] = `{"version":"1.0.99","json":{"error":"rate_limit"},"exit_code":1}`
	if code, out, errOut := f.run(t, "news", "what shipped?", "--vendor", "grok"); code != 1 || !strings.Contains(errOut, "rate_limit") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	runs := func() int {
		n := 0
		for _, r := range fakecli.Records(t, f.record) {
			if r.Mode == "headless" {
				n++
			}
		}
		return n
	}
	if n := runs(); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	if code, _, _ := f.run(t, "news", "what shipped?", "--vendor", "grok"); code == 0 {
		t.Error("the second borrow ran")
	}
	if n := runs(); n != 1 {
		t.Errorf("runs after the limit = %d, want the router to have skipped grok", n)
	}
}
