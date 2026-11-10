package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/claudetrust"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
)

// TestNoBackupChange (T6977): a change to Claude's .claude.json takes no copy
// of the file, only with the reverse that undoes it without one; its undo,
// before or after the file changed, puts back the one key, and a dry run
// prints no content.
func TestNoBackupChange(t *testing.T) {
	ctx := context.Background()
	const folder = "/work/repo"
	const orig = `{"history": "SECRET-MARKER", "projects": {}}`
	set := func(b []byte) ([]byte, error) { return claudetrust.Set(b, folder) }
	rev := claudetrust.Reverse(claudetrust.NoEntry, folder)

	t.Run("refused without its reverse, or its reverse without it", func(t *testing.T) {
		f := setup(t)
		file := filepath.Join(f.dir, ".claude.json")
		if err := os.WriteFile(file, []byte(orig), 0o600); err != nil {
			t.Fatal(err)
		}
		h := patch.Hash([]byte(orig))
		for _, p := range []patch.Patch{
			{File: file, ExpectedHash: h, Transform: set, NoBackup: true},
			{File: file, ExpectedHash: h, Transform: set, NoBackup: true, Reverse: ReverseAggyHooks},
			{File: file, ExpectedHash: h, Transform: set, Reverse: rev},
		} {
			if _, err := f.m.Apply(ctx, NewRunID(), p); err == nil {
				t.Errorf("applied %+v", p)
			}
		}
		if b, _ := os.ReadFile(file); string(b) != orig {
			t.Error("a refused change wrote")
		}
	})
	for _, since := range []bool{false, true} {
		name := map[bool]string{false: "undo as written", true: "undo after Claude wrote"}[since]
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			file := filepath.Join(f.dir, ".claude.json")
			if err := os.WriteFile(file, []byte(orig), 0o600); err != nil {
				t.Fatal(err)
			}
			run := NewRunID()
			c, err := f.m.Apply(ctx, run, patch.Patch{File: file, ExpectedHash: patch.Hash([]byte(orig)), Transform: set, NoBackup: true, Reverse: rev,
				Summary: "set the key"})
			if err != nil || c.Status != store.ChangeApplied || c.BackupPath != "" {
				t.Fatalf("%+v %v", c, err)
			}
			if entries, _ := os.ReadDir(filepath.Join(f.state, BackupDirName)); len(entries) > 0 {
				if more, _ := os.ReadDir(filepath.Join(f.state, BackupDirName, entries[0].Name())); len(more) > 0 {
					t.Errorf("a backup was taken: %v", more)
				}
			}
			if since {
				b, _ := os.ReadFile(file)
				if err := os.WriteFile(file, []byte(strings.Replace(string(b), `"history"`, `"later": 1, "history"`, 1)), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out strings.Builder
			if _, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{DryRun: true}, &out); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "SECRET-MARKER") || !strings.Contains(out.String(), "put back") {
				t.Errorf("dry run: %q", out.String())
			}
			if err := f.m.Revert(ctx, c.ChangeID); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(file)
			if st, _ := claudetrust.State(b, folder); st != claudetrust.NoEntry {
				t.Errorf("after undo %q: %s", st, b)
			}
			if since != strings.Contains(string(b), `"later"`) {
				t.Errorf("what Claude wrote since: %s", b)
			}
		})
	}
}
