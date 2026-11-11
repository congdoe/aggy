package manifest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter/hermes"
	"github.com/congdoe/aggy/internal/patch"
	"github.com/congdoe/aggy/internal/store"
)

// TestHermesBlockUndo (T6933): aggy's block in its Hermes profile's
// config.yaml is written with no backup, since the file's seeded lines may
// hold a provider's key, and undone by taking the block out, before or after
// Hermes wrote the file; a dry run prints none of the file.
func TestHermesBlockUndo(t *testing.T) {
	ctx := context.Background()
	const orig = "model:\n  default: x\n  api_key: SECRET-MARKER\n"
	block, err := hermes.Block{Bin: "/bin/aggy", Home: "/h/.aggy", Profile: "aggy-0123456789ab"}.Render()
	if err != nil {
		t.Fatal(err)
	}
	set := func(b []byte) ([]byte, error) { return hermes.SetBlock(b, block) }
	for _, since := range []bool{false, true} {
		name := map[bool]string{false: "undo as written", true: "undo after Hermes wrote"}[since]
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			file := filepath.Join(f.dir, "config.yaml")
			if err := os.WriteFile(file, []byte(orig), 0o600); err != nil {
				t.Fatal(err)
			}
			c, err := f.m.Apply(ctx, NewRunID(), patch.Patch{File: file, ExpectedHash: patch.Hash([]byte(orig)), Transform: set,
				NoBackup: true, Reverse: hermes.Reverse, Summary: "aggy's block"})
			if err != nil || c.Status != store.ChangeApplied || c.BackupPath != "" {
				t.Fatalf("%+v %v", c, err)
			}
			if since {
				b, _ := os.ReadFile(file)
				if err := os.WriteFile(file, append([]byte("display:\n  skin: dark\n"), b...), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var out strings.Builder
			if _, err := f.m.Unwire(ctx, Selection{All: true}, UnwireOptions{DryRun: true}, &out); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "SECRET-MARKER") || !strings.Contains(out.String(), "take aggy's block out") {
				t.Errorf("dry run: %q", out.String())
			}
			if err := f.m.Revert(ctx, c.ChangeID); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(file)
			want := orig
			if since {
				want = "display:\n  skin: dark\n" + orig
			}
			if string(b) != want {
				t.Errorf("after undo:\n%s\nwant:\n%s", b, want)
			}
		})
	}
}
