package parity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/patch"
)

// TestAttributionHookPatch (T6692): only a missing hook, or aggy's own,
// gives a patch; anything else at the path, or a hooks folder that is a
// link, is refused.
func TestAttributionHookPatch(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(t *testing.T, hooks string)
		want    string // ExpectedHash
		wantErr string
	}{
		{name: "missing", setup: func(*testing.T, string) {}, want: patch.NoFile},
		{name: "no hooks folder", setup: func(t *testing.T, hooks string) { must(t, os.Remove(hooks)) }, want: patch.NoFile},
		{name: "aggy's", setup: func(t *testing.T, hooks string) {
			must(t, os.WriteFile(filepath.Join(hooks, AttributionHookName), []byte(AttributionHook), 0o755))
		}, want: patch.Hash([]byte(AttributionHook))},
		{name: "another hook", setup: func(t *testing.T, hooks string) {
			must(t, os.WriteFile(filepath.Join(hooks, AttributionHookName), []byte("#!/bin/sh\n"), 0o755))
		}, wantErr: "never replaces a hook"},
		{name: "dangling link", setup: func(t *testing.T, hooks string) {
			must(t, os.Symlink(filepath.Join(t.TempDir(), "nowhere"), filepath.Join(hooks, AttributionHookName)))
		}, wantErr: "never replaces a hook"},
		{name: "linked hooks folder", setup: func(t *testing.T, hooks string) {
			must(t, os.Remove(hooks))
			must(t, os.Symlink(t.TempDir(), hooks))
		}, wantErr: "not a real directory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hooks := filepath.Join(t.TempDir(), "hooks")
			must(t, os.Mkdir(hooks, 0o755))
			tt.setup(t, hooks)
			p, err := AttributionHookPatch(filepath.Join(hooks, AttributionHookName))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || p.ExpectedHash != tt.want || p.Mode != 0o755 {
				t.Fatalf("patch = %+v, %v", p, err)
			}
		})
	}
}
