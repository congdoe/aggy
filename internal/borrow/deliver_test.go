//go:build unix

package borrow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

// imageRun runs a Codex image borrow of n images to out, in w's repo.
func imageRun(t *testing.T, w *world, out string, n int) Result {
	t.Helper()
	w.scenario[adapter.Codex] = `{"images":` + string(rune('0'+n)) + `}`
	req := w.req(adapter.Image, "a hero image")
	req.Vendor = "codex"
	req.Media = &Media{Out: filepath.Join(w.repo, out), N: n}
	res, err := w.borrower().Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return res[0]
}

// TestDeliverNeverWritesThroughLinks (B15-1, B15-3, B15-5): a cloned repo
// can plant symlinks at --out, at a sidecar, or on the way to either;
// aggy writes through none of them, makes no dir outside the repo, and
// never writes into .git.
func TestDeliverNeverWritesThroughLinks(t *testing.T) {
	tests := []struct {
		name  string
		plant func(t *testing.T, repo, outside string)
		out   string
		want  string
	}{
		{"out is a link", func(t *testing.T, repo, outside string) {
			link(t, filepath.Join(outside, "zshrc"), filepath.Join(repo, "public", "hero.png"))
		}, "public/hero.png", "not a regular file"},
		{"sidecar is a link", func(t *testing.T, repo, outside string) {
			link(t, filepath.Join(outside, "zshrc"), filepath.Join(repo, ".aggy", "assets", "public", "hero.png.json"))
		}, "public/hero.png", "not a regular file"},
		{"assets dir is a link", func(t *testing.T, repo, outside string) {
			link(t, outside, filepath.Join(repo, ".aggy", "assets"))
		}, "public/a/b/hero.png", "a symlink"},
		// F2: no symlink under .aggy, even one that stays in the repo.
		{"assets dir links inside the repo", func(t *testing.T, repo, _ string) {
			if err := os.MkdirAll(filepath.Join(repo, "elsewhere"), 0o700); err != nil {
				t.Fatal(err)
			}
			link(t, filepath.Join(repo, "elsewhere"), filepath.Join(repo, ".aggy", "assets"))
		}, "hero.png", "a symlink"},
		{"out dir is a link out", func(t *testing.T, repo, outside string) {
			link(t, outside, filepath.Join(repo, "public"))
		}, "public/a/b/hero.png", "outside"},
		{"out dir is a link to .git", func(t *testing.T, repo, _ string) {
			link(t, filepath.Join(repo, ".git"), filepath.Join(repo, "pub"))
		}, "pub/hooks/x.png", "not where aggy writes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			outside := t.TempDir()
			if err := os.WriteFile(filepath.Join(outside, "zshrc"), []byte("export PATH"), 0o600); err != nil {
				t.Fatal(err)
			}
			tt.plant(t, w.repo, outside)
			r := imageRun(t, w, tt.out, 1)
			if r.Status != "failed" || !strings.Contains(r.Detail, tt.want) {
				t.Fatalf("result %+v", r)
			}
			if b, _ := os.ReadFile(filepath.Join(outside, "zshrc")); string(b) != "export PATH" {
				t.Errorf("the outside file was written: %q", b)
			}
			es, _ := os.ReadDir(outside)
			if len(es) != 1 {
				t.Errorf("made %d entries outside the repo", len(es)-1)
			}
			if _, err := os.Stat(filepath.Join(w.repo, ".git", "hooks")); err == nil {
				t.Error("wrote into .git")
			}
		})
	}
}

// TestDeliverKeepsExistingFiles (B15-5): only --out itself may be
// replaced; a variant or a changed extension takes the next free name.
func TestDeliverKeepsExistingFiles(t *testing.T) {
	w := newWorld(t)
	for _, f := range []string{"hero.png", "hero-2.png"} {
		if err := os.WriteFile(filepath.Join(w.repo, f), []byte("mine"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	r := imageRun(t, w, "hero.png", 2)
	var got []string
	for _, a := range r.Artifacts {
		got = append(got, filepath.Base(a.Path))
	}
	if r.Status != "done" || strings.Join(got, ",") != "hero.png,hero-3.png" {
		t.Fatalf("wrote %q: %+v", got, r)
	}
	if b, _ := os.ReadFile(filepath.Join(w.repo, "hero-2.png")); string(b) != "mine" {
		t.Errorf("hero-2.png overwritten: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(w.repo, "hero.png")); sniff(b) != PNG {
		t.Errorf("--out itself not replaced: %q", b)
	}
}

func TestReservedCaseInsensitive(t *testing.T) {
	for _, out := range []string{"/r/.GIT/x.png", "/r/.Git/hooks/x.png", "/r/.AGGY/assets/x.png"} {
		if err := (&Media{Out: out}).validate(Request{Kind: adapter.Image, Repo: "/r"}); err == nil {
			t.Errorf("%s accepted", out)
		}
	}
}

// TestEditOnlyOwnSessions (B15-6): a sidecar from a cloned repo can name
// any session; --edit resumes only one an aggy media borrow recorded.
func TestEditOnlyOwnSessions(t *testing.T) {
	w := newWorld(t)
	req := w.req(adapter.Image, "darker")
	req.Vendor = "codex"
	req.Media = &Media{Out: filepath.Join(w.repo, "hero.png"), Resume: "019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b"}
	if _, err := w.borrower().Run(context.Background(), req); err == nil || !strings.Contains(err.Error(), "not made by an aggy image borrow") {
		t.Fatalf("err = %v", err)
	}
	if len(w.records()) != 0 {
		t.Error("a child ran")
	}
}

// TestGateCountsWithoutReading (B15-4): past the cap, files are counted,
// not read: an unreadable fifth file is not an error.
func TestGateCountsWithoutReading(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	dir := t.TempDir()
	var arts []adapter.Artifact
	for i := range MaxImages + 1 {
		p := filepath.Join(dir, string(rune('a'+i))+".png")
		write(pngBytes)(t, p)
		arts = append(arts, adapter.Artifact{Path: p, Kind: "image"})
	}
	if err := os.Chmod(arts[MaxImages].Path, 0); err != nil {
		t.Fatal(err)
	}
	ok, refused, made := gate(arts, time.Time{}, false)
	if len(ok) != MaxImages || made != MaxImages+1 || len(refused) != 1 || !strings.Contains(refused[0], "1 more not kept") {
		t.Fatalf("gate = %d kept, %q, %d made", len(ok), refused, made)
	}
}

func link(t *testing.T, target, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
}
