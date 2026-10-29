package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestToolSwitchProxy (T6780): the proxy confirmation is kept only on an
// "on" for one named vendor.
func TestToolSwitchProxy(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	for _, sw := range []ToolSwitch{
		{Tool: "a", Vendor: "claude", On: true, Proxy: true},
		{Tool: "b", Vendor: "", On: true, Proxy: true},
		{Tool: "c", Vendor: "claude", On: false, Proxy: true},
		{Tool: "d", Vendor: "claude", On: true},
	} {
		if err := s.PutToolSwitch(ctx, sw); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ToolSwitches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, sw := range got {
		if sw.Proxy != (sw.Tool == "a") {
			t.Errorf("%s: proxy %v", sw.Tool, sw.Proxy)
		}
	}
}

// TestPutToolSwitch: a setting replaces every narrower setting of the
// same tool that it covers, and nothing else.
func TestPutToolSwitch(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	now := time.UnixMilli(1_700_000_000_000)
	put := func(tool, vendor, repo string, on bool) {
		t.Helper()
		if err := s.PutToolSwitch(ctx, ToolSwitch{Tool: tool, Vendor: vendor, Repo: repo, On: on, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	put("demo", "claude", "/r/a", true)
	put("demo", "", "/r/b", true)
	put("demo", "codex", "", true)
	put("other", "claude", "/r/a", true)
	put("demo", "claude", "", false) // covers claude in every repo
	got, err := s.ToolSwitches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []ToolSwitch{
		{Tool: "demo", Vendor: "claude", Repo: "", On: false, At: now},
		{Tool: "demo", Vendor: "codex", Repo: "", On: true, At: now},
		{Tool: "demo", Vendor: "", Repo: "/r/b", On: true, At: now},
		{Tool: "other", Vendor: "claude", Repo: "/r/a", On: true, At: now},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if !got[i].At.Equal(now) {
			t.Errorf("row %d at %v", i, got[i].At)
		}
		got[i].At = now
		if got[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	put("demo", "", "", false) // covers every demo setting
	if got, _ := s.ToolSwitches(ctx); len(got) != 2 || got[0].Tool != "demo" || got[0].On || got[1].Tool != "other" {
		t.Errorf("after off everywhere: %+v", got)
	}
}
