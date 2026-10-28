package probe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

// Regression tests for the PR #5 review (6, 9). Each fails on ed38103.

// TestUnreadableVersionDoesNotFailDetect (9): a --version format aggy
// cannot read leaves Version empty for adapter.VersionWarning, and the
// probe still reports capabilities; x.y is a version.
func TestUnreadableVersionDoesNotFailDetect(t *testing.T) {
	for _, tt := range []struct{ out, want string }{
		{"grok build xyz\n", ""},
		{"tool 1.0.0\nnode 2.0.0\n", ""},
		{"grok 1.1\n", "1.1"},
	} {
		run := func(_ context.Context, c adapter.Command) (adapter.Output, error) {
			if c.Args[0] == "--version" {
				return adapter.Output{Stdout: []byte(tt.out)}, nil
			}
			return adapter.Output{Stdout: []byte("  --session-id <uuid>\n")}, nil
		}
		p, err := Detect(context.Background(), run, adapter.Grok, "/x/grok")
		if err != nil || p.Version != tt.want || !Has(p, CapSessionID) {
			t.Errorf("%q: Detect = %+v, %v", tt.out, p, err)
		}
	}
	// The warning shows the first line the CLI printed, capped, or
	// "[redacted]" for a secret-shaped one.
	long := "grok" + strings.Repeat(" ab", 70) + "\n"
	for _, tt := range []struct{ out, want string }{
		{"\n  grok build xyz  \nmore\n", "grok build xyz"},
		{long, long[:maxVersionLine] + "..."},
		{"sk-ant-api03-" + strings.Repeat("a", 40) + "\n", "[redacted]"},
	} {
		run := func(_ context.Context, c adapter.Command) (adapter.Output, error) {
			return adapter.Output{Stdout: []byte(tt.out)}, nil
		}
		p, _ := Detect(context.Background(), run, adapter.Grok, "/x/grok")
		if p.Version != "" || p.VersionLine != tt.want {
			t.Errorf("%q: VersionLine = %q, want %q", tt.out, p.VersionLine, tt.want)
		}
	}
	f := setup(t)
	run := func(_ context.Context, c adapter.Command) (adapter.Output, error) {
		if c.Args[0] == "--version" {
			return adapter.Output{Stdout: []byte("weird\n")}, nil
		}
		return adapter.Output{Stdout: []byte("  --tools <list>\n")}, nil
	}
	pr := &Prober{Store: f.store, Run: run}
	p, cached, err := pr.Probe(context.Background(), adapter.Grok, "/x/grok")
	if err != nil || cached || p.Version != "" || !Has(p, CapTools) {
		t.Errorf("Prober = %+v, %v, %v", p, cached, err)
	}
}

// TestIgnoreUserConfigCapability (6): recorded when codex --help or codex
// exec --help lists --ignore-user-config.
func TestIgnoreUserConfigCapability(t *testing.T) {
	with := "Usage: codex exec [OPTIONS]\n\nOptions:\n  --json\n  --ignore-user-config\n"
	for _, tt := range []struct {
		exec string
		want bool
	}{{with, true}, {"Usage: codex exec\n  --json\n", false}} {
		p, err := Detect(context.Background(), codexRun(tt.exec, adapter.Output{}), adapter.Codex, "/x/codex")
		if err != nil || Has(p, CapIgnoreUserConfig) != tt.want {
			t.Errorf("caps = %v, %v; want ignore-user-config %v", p.Capabilities, err, tt.want)
		}
	}
}

// TestKernelSandbox (G32-1): Grok's sandbox fails open, so aggy accepts
// only a host that has one: darwin, or Linux with Landlock listed.
func TestKernelSandbox(t *testing.T) {
	lsm := func(s string, err error) func() ([]byte, error) {
		return func() ([]byte, error) { return []byte(s), err }
	}
	tests := []struct {
		goos string
		read func() ([]byte, error)
		ok   bool
	}{
		{"darwin", nil, true},
		{"linux", lsm("lockdown,capability,landlock,yama,apparmor\n", nil), true},
		{"linux", lsm("landlock", nil), true},
		{"linux", lsm("lockdown,capability,yama,apparmor\n", nil), false},
		{"linux", lsm("lockdown,capability,notlandlock", nil), false},
		{"linux", lsm("", nil), false},
		{"linux", lsm("", errors.New("no securityfs")), false},
		{"freebsd", nil, false},
		{"windows", nil, false},
	}
	for _, tt := range tests {
		if err := KernelSandbox(tt.goos, tt.read); (err == nil) != tt.ok {
			t.Errorf("%s: %v, want ok %v", tt.goos, err, tt.ok)
		}
	}
}
