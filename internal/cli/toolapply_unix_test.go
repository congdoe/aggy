//go:build unix

package cli

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/home"
	"github.com/congdoe/aggy/internal/probe"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
)

// TestToolSwitchNotifies (T6729): aggy tool on|off tells the running
// sessions its setting can reach (the vendor on screen, the repo), and
// no other.
func TestToolSwitchNotifies(t *testing.T) {
	isolate(t)
	d, err := os.MkdirTemp("/tmp", "aggyn") // a socket path must be short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	d, _ = filepath.EvalSymlinks(d)
	t.Setenv(home.Env, d)
	repo1, repo2 := switchRepo(t, ""), switchRepo(t, "")
	t.Chdir(repo1)

	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sessions := []struct {
		id, name, vendor, dir string
		row                   bool
	}{
		{"11111111-2222-4333-8444-000000000001", "claude-repo1", "claude", repo1, true},
		{"11111111-2222-4333-8444-000000000002", "codex-repo1", "codex", repo1, true},
		{"11111111-2222-4333-8444-000000000003", "claude-repo2", "claude", repo2, true},
		{"11111111-2222-4333-8444-000000000004", "no-row", "claude", repo1, false},
	}
	var mu sync.Mutex
	var told []string
	now := time.Now()
	for _, ss := range sessions {
		if ss.row {
			if err := s.InsertParent(ctx, store.Parent{ID: ss.id, Dir: ss.dir, LastVendor: ss.vendor, CreatedAt: now, LastActive: now}); err != nil {
				t.Fatal(err)
			}
		}
		srv, err := supervisor.Listen(filepath.Join(d, "run"), ss.id, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Close() })
		srv.SetTools(func() {
			mu.Lock()
			defer mu.Unlock()
			told = append(told, ss.name)
		})
	}
	_ = s.Close()

	steps := []struct {
		args []string
		want []string
	}{
		{[]string{"on", "context7", "--vendor", "claude", "--repo", repo1}, []string{"claude-repo1"}},
		{[]string{"off", "context7", "--vendor", "claude"}, []string{"claude-repo1", "claude-repo2"}},
		{[]string{"on", "context7", "--repo", repo2}, []string{"claude-repo2"}},
		{[]string{"off", "context7"}, []string{"claude-repo1", "claude-repo2", "codex-repo1"}},
	}
	for _, st := range steps {
		mu.Lock()
		told = nil
		mu.Unlock()
		code, out, errOut := toolRun("", false, st.args...)
		if code != 0 {
			t.Fatalf("%v: exit %d %q", st.args, code, errOut)
		}
		mu.Lock()
		got := slices.Sorted(slices.Values(told))
		mu.Unlock()
		if !slices.Equal(got, st.want) {
			t.Errorf("%v told %q, want %q", st.args, got, st.want)
		}
		if !strings.Contains(out, "running aggy session") {
			t.Errorf("%v: output %q", st.args, out)
		}
	}
}

// TestSessionToolSet (T6729): what a session gets, as its launch would
// wire it: nothing for Grok (gated) or a Codex without -c, nothing a
// repo's tools_off turns off, and a session's --tool.
func TestSessionToolSet(t *testing.T) {
	isolate(t)
	repo, off := switchRepo(t, ""), switchRepo(t, "tools_off = [\"context7\", \"rtk\"]\n")
	if code, _, errOut := toolRun("", false, "on", "context7"); code != 0 {
		t.Fatalf("on: %q", errOut)
	}
	ctx := context.Background()
	codexCaps, claudeCaps := []string{probe.CapConfigOverride}, []string{probe.CapSettings}
	tests := []struct {
		name    string
		v       adapter.Vendor
		caps    []string
		dir     string
		session []string
		want    supervisor.ToolSet
	}{
		{"claude", adapter.Claude, claudeCaps, repo, nil, supervisor.ToolSet{"context7": "mcp"}},
		{"claude --tool rtk", adapter.Claude, claudeCaps, repo, []string{"rtk"}, supervisor.ToolSet{"context7": "mcp", "rtk": "hook"}},
		{"claude without --settings", adapter.Claude, nil, repo, []string{"rtk"}, supervisor.ToolSet{"context7": "mcp"}},
		{"codex", adapter.Codex, codexCaps, repo, nil, supervisor.ToolSet{"context7": "mcp"}},
		{"codex without -c", adapter.Codex, nil, repo, nil, supervisor.ToolSet{}},
		{"codex, rtk's hook refused", adapter.Codex, codexCaps, repo, []string{"rtk"}, supervisor.ToolSet{"context7": "mcp"}},
		{"grok (gated)", adapter.Grok, nil, repo, nil, supervisor.ToolSet{}},
		{"tools_off", adapter.Claude, claudeCaps, off, []string{"rtk"}, supervisor.ToolSet{}},
	}
	for _, tt := range tests {
		got, err := sessionToolSet(ctx, tt.v, tt.caps, tt.dir, tt.session)
		if err != nil || !maps.Equal(got, tt.want) {
			t.Errorf("%s: %v, %v; want %v", tt.name, got, err, tt.want)
		}
	}
}
