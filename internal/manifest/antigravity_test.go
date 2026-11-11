package manifest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter/antigravity"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
)

// TestAntigravityHooksUndo (T6957): aggy's entry in a folder's
// .agents/hooks.json is undone by taking the entry out, whether or not the
// person edited the file since, and a file aggy created goes away.
func TestAntigravityHooksUndo(t *testing.T) {
	ctx := context.Background()
	entry, err := antigravity.Entry("/bin/aggy")
	if err != nil {
		t.Fatal(err)
	}
	set := func(b []byte) ([]byte, error) { return antigravity.SetHooks(b, entry) }
	const theirs = "{\n  \"mine\": {\"Stop\": []}\n}\n"
	for _, tt := range []struct {
		name  string
		orig  string // "" for no file
		since bool
	}{
		{"merged, undo as written", theirs, false},
		{"merged, undo after an edit", theirs, true},
		{"created", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := setup(t)
			file := filepath.Join(f.dir, ".agents", "hooks.json")
			p := patch.Patch{File: file, ExpectedHash: patch.NoFile, Transform: func(b []byte) ([]byte, error) { return set(nil) },
				Mode: 0o644, Reverse: antigravity.Reverse, Summary: "aggy's hooks entry"}
			if tt.orig != "" {
				if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(tt.orig), 0o600); err != nil {
					t.Fatal(err)
				}
				p.ExpectedHash, p.Transform = patch.Hash([]byte(tt.orig)), set
			}
			c, err := f.m.Apply(ctx, NewRunID(), p)
			if err != nil || c.Status != store.ChangeApplied {
				t.Fatalf("%+v %v", c, err)
			}
			want := tt.orig
			if tt.since {
				b, _ := os.ReadFile(file)
				edited := strings.Replace(string(b), "\"mine\"", "\"yours\": {},\n  \"mine\"", 1)
				if err := os.WriteFile(file, []byte(edited), 0o600); err != nil {
					t.Fatal(err)
				}
				want = strings.Replace(theirs, "\"mine\"", "\"yours\": {},\n  \"mine\"", 1)
			}
			var out strings.Builder
			if _, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{DryRun: true}, &out); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "-  \"aggy\": ") && !strings.Contains(out.String(), "-{") {
				t.Errorf("dry run: %q", out.String())
			}
			if err := f.m.Revert(ctx, c.ChangeID); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(file)
			if tt.orig == "" {
				if err == nil {
					t.Errorf("the file aggy made is still there: %s", b)
				}
				return
			}
			if string(b) != want {
				t.Errorf("after undo:\n%s\nwant:\n%s", b, want)
			}
		})
	}
}

// TestAntigravityHooksUndoRefusesLink (#141 review, M1): once .agents is a
// link, aggy unwire refuses to touch the file it leads to.
func TestAntigravityHooksUndoRefusesLink(t *testing.T) {
	ctx := context.Background()
	entry, err := antigravity.Entry("/bin/aggy")
	if err != nil {
		t.Fatal(err)
	}
	f := setup(t)
	file := filepath.Join(f.dir, ".agents", "hooks.json")
	c, err := f.m.Apply(ctx, NewRunID(), patch.Patch{File: file, ExpectedHash: patch.NoFile, Mode: 0o644, Reverse: antigravity.Reverse,
		Transform: func([]byte) ([]byte, error) { return antigravity.SetHooks(nil, entry) }})
	if err != nil {
		t.Fatal(err)
	}
	// The real folder moves elsewhere, and .agents becomes a link to it.
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Rename(filepath.Dir(file), outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Dir(file)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(outside, "hooks.json"))
	if err := f.m.Revert(ctx, c.ChangeID); err == nil || !errors.Is(err, antigravity.ErrHooksPath) {
		t.Fatalf("Revert = %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(outside, "hooks.json")); string(after) != string(before) {
		t.Errorf("the file behind the link changed:\n%s", after)
	}
}
