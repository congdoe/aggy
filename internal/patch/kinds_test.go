//go:build unix

package patch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/congdoe/aggy/internal/safefs"
)

// node is what is at a path, never followed: its type, permission bits,
// and a file's bytes or a link's target.
type node struct {
	typ  fs.FileMode
	perm fs.FileMode
	data string
}

func lnode(t *testing.T, path string) *node {
	t.Helper()
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	n := &node{typ: fi.Mode().Type(), perm: fi.Mode().Perm()}
	if fi.Mode()&fs.ModeSymlink != 0 {
		n.data, err = os.Readlink(path)
		n.perm = 0 // a link's own mode is not part of it
	} else {
		var b []byte
		b, err = os.ReadFile(path)
		n.data = string(b)
	}
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func sameNode(a, b *node) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// TestLinkAndRemoveKinds applies each link and removal kind, checks what
// is left, and reverts it byte for byte: the same file and mode, or the
// same link.
func TestLinkAndRemoveKinds(t *testing.T) {
	type start int
	const (
		none start = iota
		file
		link
		dangling
	)
	cases := []struct {
		name   string
		start  start
		link   string // the patch's Link; "" means a Remove patch
		action Action
	}{
		{"create a link", none, "store/aggy", Create},
		{"replace a file with a link", file, "store/aggy", Modify},
		{"replace a link with a link", link, "store/aggy", Modify},
		{"remove a file", file, "", Delete},
		{"remove a link", link, "", Delete},
		{"remove a dangling link", dangling, "", Delete},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, nil)
			path := filepath.Join(e.dir, "skills", "aggy")
			other := filepath.Join(e.dir, "other.md")
			must(t, os.WriteFile(other, []byte("other\n"), 0o644))
			if tc.start != none {
				must(t, os.MkdirAll(filepath.Dir(path), 0o755))
			}
			switch tc.start {
			case file:
				must(t, os.WriteFile(path, []byte("old\r\nbytes"), 0o640))
				must(t, os.Chmod(path, 0o640))
			case link:
				must(t, os.Symlink(other, path))
			case dangling:
				must(t, os.Symlink("../gone", path))
			}
			before := lnode(t, path)
			st, err := ReadState(path)
			must(t, err)
			p := Patch{File: path, ExpectedHash: st.Hash, Link: tc.link, Remove: tc.link == ""}

			rec, err := Apply(p, e.backups)
			if err != nil {
				t.Fatal(err)
			}
			if !rec.Changed || !rec.NoFollow() {
				t.Errorf("record = %+v", rec)
			}
			got := lnode(t, path)
			switch {
			case tc.link != "":
				if got == nil || got.typ != fs.ModeSymlink || got.data != tc.link || rec.AfterHash != LinkHash(tc.link) {
					t.Errorf("after apply: %+v, %+v", got, rec)
				}
			case got != nil:
				t.Errorf("after remove: %+v", got)
			}
			if b, _ := os.ReadFile(other); string(b) != "other\n" {
				t.Errorf("the link's target changed: %q", b)
			}
			// Applying again is a no-op.
			if again, err := Apply(p, e.backups); err != nil || again.Changed {
				t.Errorf("second apply = %+v, %v", again, err)
			}

			if err := Revert(rec); err != nil {
				t.Fatal(err)
			}
			if after := lnode(t, path); !sameNode(before, after) {
				t.Errorf("revert left %+v, want %+v", after, before)
			}
			if tc.start == none {
				if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("the directory made for the link is still there: %v", err)
				}
			}
			if b, _ := os.ReadFile(other); string(b) != "other\n" {
				t.Errorf("the revert changed the link's target: %q", b)
			}
		})
	}
}

// TestLinkRevertDrift: a link retargeted, or replaced by a file, after
// aggy made it is drift; Revert leaves it, and RevertForce saves it.
func TestLinkRevertDrift(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(path string) error
	}{
		{"retargeted", func(p string) error {
			if err := os.Remove(p); err != nil {
				return err
			}
			return os.Symlink("elsewhere", p)
		}},
		{"replaced by a file", func(p string) error {
			if err := os.Remove(p); err != nil {
				return err
			}
			return os.WriteFile(p, []byte("mine\n"), 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, nil)
			path := filepath.Join(e.dir, "aggy")
			rec, err := Apply(Patch{File: path, ExpectedHash: NoFile, Link: "target"}, e.backups)
			must(t, err)
			must(t, tc.after(path))
			edited := lnode(t, path)
			if err := Revert(rec); !errors.Is(err, ErrDrift) {
				t.Fatalf("Revert = %v, want drift", err)
			}
			if got := lnode(t, path); !sameNode(got, edited) {
				t.Errorf("a refused revert changed the path: %+v", got)
			}
			saved, err := RevertForce(rec, e.backups)
			must(t, err)
			if lnode(t, path) != nil {
				t.Error("a forced revert left the path")
			}
			b, err := os.ReadFile(saved)
			if err != nil || string(b) != edited.data {
				t.Errorf("saved %q, %v, want %q", b, err, edited.data)
			}
		})
	}
}

// TestLinkRefusesCredentials: a link to a credential file, by absolute or
// relative target, and a link that would replace one, are refused by
// Plan and Apply, and nothing is written.
func TestLinkRefusesCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	cred := filepath.Join(home, ".codex", "auth.json")
	must(t, os.MkdirAll(filepath.Dir(cred), 0o700))
	must(t, os.WriteFile(cred, []byte(`{"token":"fake"}`), 0o600))
	claudeCred := filepath.Join(home, ".claude", ".credentials.json")
	must(t, os.MkdirAll(filepath.Dir(claudeCred), 0o700))
	must(t, os.WriteFile(claudeCred, []byte(`{}`), 0o600))
	elsewhere := filepath.Join(home, "x", "y", "aggy")
	for _, tc := range []struct {
		name string
		p    Patch
	}{
		{"absolute target", Patch{File: elsewhere, ExpectedHash: NoFile, Link: cred}},
		{"relative target", Patch{File: elsewhere, ExpectedHash: NoFile, Link: "../../.codex/auth.json"}},
		{"missing credential target", Patch{File: elsewhere, ExpectedHash: NoFile, Link: filepath.Join(home, ".grok", "auth.json")}},
		{"replace a credential", Patch{File: claudeCred, ExpectedHash: Hash([]byte(`{}`)), Link: "/tmp/x"}},
		{"remove a credential", Patch{File: claudeCred, ExpectedHash: Hash([]byte(`{}`)), Remove: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Plan(tc.p); !errors.Is(err, safefs.ErrAuthPath) {
				t.Errorf("Plan = %v, want ErrAuthPath", err)
			}
			if _, err := Apply(tc.p, filepath.Join(home, "b")); !errors.Is(err, safefs.ErrAuthPath) {
				t.Errorf("Apply = %v, want ErrAuthPath", err)
			}
			if _, err := os.Lstat(filepath.Join(home, "x")); !errors.Is(err, fs.ErrNotExist) {
				t.Error("a refused link made directories")
			}
			if b, _ := os.ReadFile(claudeCred); string(b) != `{}` {
				t.Error("the credential changed")
			}
		})
	}
	// safefs.Symlink refuses on its own too.
	if err := safefs.Symlink(cred, elsewhere, nil); !errors.Is(err, safefs.ErrAuthPath) {
		t.Errorf("safefs.Symlink = %v, want ErrAuthPath", err)
	}
}

// TestLinkPlanWritesNothing: planning a link in missing directories and
// a removal makes no directory, link, or backup.
func TestLinkPlanWritesNothing(t *testing.T) {
	e := setup(t, ptr("keep\n"))
	path := filepath.Join(e.dir, "a", "b", "aggy")
	pl, err := Plan(Patch{File: path, ExpectedHash: NoFile, Link: e.file})
	if err != nil || pl.Action != Create || len(pl.CreatedDirs) != 2 {
		t.Fatalf("Plan = %+v, %v", pl, err)
	}
	pl, err = Plan(Patch{File: e.file, ExpectedHash: Hash([]byte("keep\n")), Remove: true})
	if err != nil || pl.Action != Delete {
		t.Fatalf("Plan remove = %+v, %v", pl, err)
	}
	for _, p := range []string{filepath.Join(e.dir, "a"), e.backups} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Plan made %s", p)
		}
	}
	if content(t, e.file) != "keep\n" {
		t.Error("Plan changed the file")
	}
}

// TestKindValidation: exactly one of Transform, Link, and Remove; a
// removal of nothing is a no-op; a directory is refused.
func TestKindValidation(t *testing.T) {
	e := setup(t, nil)
	for _, p := range []Patch{
		{File: e.file, ExpectedHash: NoFile},
		{File: e.file, ExpectedHash: NoFile, Transform: addLine, Link: "x"},
		{File: e.file, ExpectedHash: NoFile, Link: "x", Remove: true},
		{File: e.dir, ExpectedHash: NoFile, Remove: true},
	} {
		if _, err := Plan(p); err == nil {
			t.Errorf("Plan(%+v) accepted", p)
		}
	}
	if pl, err := Plan(Patch{File: e.file, ExpectedHash: NoFile, Remove: true}); err != nil || pl.Action != NoOp {
		t.Errorf("removing nothing = %+v, %v", pl, err)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
