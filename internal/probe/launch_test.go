package probe

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// countRunner answers a grok probe from text and counts the processes it
// would have started. fail makes every help run exit 1.
type countRunner struct {
	runs    int
	version string
	fail    bool
}

func (c *countRunner) run(_ context.Context, cmd adapter.Command) (adapter.Output, error) {
	c.runs++
	if strings.Join(cmd.Args, " ") == "--version" {
		return adapter.Output{Stdout: []byte("grok " + c.version + "\n")}, nil
	}
	if c.fail {
		return adapter.Output{ExitCode: 1, Stderr: []byte("transient failure")}, nil
	}
	return adapter.Output{Stdout: []byte("Usage: grok\n  -p, --single <PROMPT>\n  --session-id <ID>\n  --add-dir <DIR>...\n  --debug [FILTER]\n")}, nil
}

// TestLaunchCachesByBinary (T6964): a launch's probe is cached by the binary's
// identity. An unchanged binary starts no process; a changed one (touched,
// replaced, or a new version) is probed again; a failed probe is not cached.
func TestLaunchCachesByBinary(t *testing.T) {
	steps := []struct {
		name    string
		change  func(t *testing.T, path string)
		version string
		fail    bool
		runs    int
		cached  bool
	}{
		{"first launch probes", nil, "1.0.70", false, 2, false},
		{"unchanged: no process", nil, "1.0.70", false, 0, true},
		{"touched: probed again", func(t *testing.T, p string) {
			if err := os.Chtimes(p, time.Now(), time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		}, "1.0.70", false, 2, false},
		{"then cached again", nil, "1.0.70", false, 0, true},
		{"replaced by a new version", func(t *testing.T, p string) {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("#!/bin/sh\n# a newer build\n"), 0o755); err != nil {
				t.Fatal(err)
			}
		}, "1.1.0", true, 2, false},
		{"a failed help was not cached", nil, "1.1.0", false, 2, false},
		{"until it succeeds", nil, "1.1.0", false, 0, true},
	}
	f := setup(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, st := range steps {
		if st.change != nil {
			st.change(t, path)
		}
		r := &countRunner{version: st.version, fail: st.fail}
		p, cached, err := Launch(ctx, f.store, r.run, adapter.Grok, path)
		if err != nil || cached != st.cached || r.runs != st.runs {
			t.Fatalf("%s: cached %v, runs %d, %v; want cached %v, runs %d", st.name, cached, r.runs, err, st.cached, st.runs)
		}
		if p.Version != st.version {
			t.Errorf("%s: version %q, want %q", st.name, p.Version, st.version)
		}
		if !st.fail && (!slices.Equal(p.ValueFlags, []string{"-p", "--single", "--session-id", "--add-dir..."}) || !Has(p, CapSessionID)) {
			t.Errorf("%s: value flags %q, caps %q", st.name, p.ValueFlags, p.Capabilities)
		}
		if slices.ContainsFunc(p.Capabilities, func(c string) bool { return strings.Contains(c, ":") }) {
			t.Errorf("%s: a stored marker leaked into the capabilities: %q", st.name, p.Capabilities)
		}
	}
}

// TestLaunchWithoutStore (T6964): with no index, the CLI is probed every time
// and nothing fails.
func TestLaunchWithoutStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grok")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		r := &countRunner{version: "1.0.70"}
		if _, cached, err := Launch(context.Background(), nil, r.run, adapter.Grok, path); err != nil || cached || r.runs != 2 {
			t.Fatalf("cached %v, runs %d, %v", cached, r.runs, err)
		}
	}
}
