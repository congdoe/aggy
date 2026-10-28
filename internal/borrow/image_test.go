package borrow

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

var (
	pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpgBytes = []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	mp4Bytes = []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00")
)

func TestMediaValidate(t *testing.T) {
	repo := "/r"
	tests := []struct {
		name string
		kind adapter.Kind
		m    Media
		want string
	}{
		{"ok", adapter.Image, Media{Out: "/r/public/hero.png", Refs: []string{"/x/s.png"}, N: 3}, ""},
		{"jpeg", adapter.Image, Media{Out: "/r/a.JPEG"}, ""},
		{"video", adapter.Video, Media{Out: "/r/intro.mp4"}, ""},
		{"relative", adapter.Image, Media{Out: "hero.png"}, "must be absolute"},
		{"outside the repo", adapter.Image, Media{Out: "/elsewhere/hero.png"}, "must be a file inside /r"},
		{"climbs out", adapter.Image, Media{Out: "/r/../hero.png"}, "must be a file inside"},
		{"the repo itself", adapter.Image, Media{Out: "/r"}, "must be a file inside"},
		{"in .git", adapter.Image, Media{Out: "/r/.git/hooks/x.png"}, "outside its .git"},
		{"in .aggy", adapter.Image, Media{Out: "/r/.aggy/assets/x.png"}, "outside its .git"},
		{"wrong ext", adapter.Image, Media{Out: "/r/hero.gif"}, "must end in .png"},
		{"video ext", adapter.Video, Media{Out: "/r/intro.png"}, "must end in .mp4"},
		{"too many", adapter.Image, Media{Out: "/r/a.png", N: MaxImages + 1}, "--n must be"},
		{"video variants", adapter.Video, Media{Out: "/r/a.mp4", N: 2}, "--n is for images"},
		{"video refs", adapter.Video, Media{Out: "/r/a.mp4", Refs: []string{"/x.png"}}, "--ref is for images"},
		{"relative ref", adapter.Image, Media{Out: "/r/a.png", Refs: []string{"s.png"}}, "--ref"},
		{"research", adapter.Research, Media{Out: "/r/a.png"}, "for image and video"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.m.validate(Request{Kind: tt.kind, Repo: repo})
			if (err == nil) != (tt.want == "") || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Fatalf("validate = %v, want %q", err, tt.want)
			}
		})
	}
}

// TestImageRefsAndVariants (T6631): --ref images reach Codex as -i,
// copied into the run dir through the gate, and --n is in the prompt.
// Grok takes no reference images, so it is passed over.
func TestImageRefsAndVariants(t *testing.T) {
	w := newWorld(t)
	ref := filepath.Join(t.TempDir(), "style.png")
	if err := os.WriteFile(ref, pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	w.cfg.Routing.Image.Vendors = []adapter.Vendor{adapter.Grok, adapter.Codex}
	w.scenario[adapter.Codex] = `{"images":3}`
	req := w.req(adapter.Image, "a hero image")
	req.Media = &Media{Out: filepath.Join(w.repo, "hero.png"), Refs: []string{ref}, N: 3}
	res, err := w.borrower().Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]
	if r.Vendor != adapter.Codex || len(r.Tried) != 1 || !strings.Contains(r.Tried[0].Reason, "reference images") {
		t.Fatalf("result %+v", r)
	}
	recs := w.records()
	rec := recs[len(recs)-1]
	refs := rec.Flags["-i"]
	if len(refs) != 1 || refs[0] == ref || filepath.Base(refs[0]) != "ref-1.png" {
		t.Errorf("-i = %q, want the staged copy", refs)
	}
	if !strings.Contains(rec.Stdin, "Make 3 separate images") || !strings.Contains(rec.Stdin, "references") {
		t.Errorf("prompt lacks the variants or refs:\n%s", rec.Stdin)
	}
}

func TestImageRefRefused(t *testing.T) {
	w := newWorld(t)
	ref := filepath.Join(t.TempDir(), "notes.png")
	if err := os.WriteFile(ref, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := w.req(adapter.Image, "a hero image")
	req.Vendor = "codex"
	req.Media = &Media{Out: filepath.Join(w.repo, "hero.png"), Refs: []string{ref}}
	if _, err := w.borrower().Run(context.Background(), req); err == nil || !strings.Contains(err.Error(), "not a PNG or JPEG") {
		t.Fatalf("err = %v", err)
	}
	if len(w.records()) != 0 {
		t.Error("a child ran")
	}
}

// TestCodexImagesCounted (T6632): every file in the thread's folder is
// collected and counted; one that is not an image is refused, and the
// count still includes it.
func TestCodexImagesCounted(t *testing.T) {
	w := newWorld(t)
	w.scenario[adapter.Codex] = `{"images":2,"session_files":[{"path":"generated_images/{session_id}/3.txt","content":"not an image"}]}`
	req := w.req(adapter.Image, "two cats")
	req.Vendor = "codex"
	res, err := w.borrower().Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]
	if r.Status != "done" || r.Made != 3 || len(r.Artifacts) != 2 || len(r.Refused) != 1 || !strings.Contains(r.Refused[0], "3.txt: refused, it is not a PNG or JPEG image") {
		t.Fatalf("result %+v", r)
	}
	row, err := w.store.GetBorrow(context.Background(), r.ID)
	if err != nil || row.ImageCount != 3 || len(row.Artifacts) != 2 {
		t.Errorf("row %+v, %v", row, err)
	}
	report, _ := os.ReadFile(r.ReportPath)
	if !strings.Contains(string(report), "- Files made: 3, kept: 2") || !strings.Contains(string(report), "- Refused file: ") {
		t.Errorf("report:\n%s", report)
	}
}

// TestGrokImagesCounted (T6633): Grok's images are found in
// sessions/<encoded cwd>/<sessionId>/images/, built from the reply's
// session id and the child's temp cwd, and pass the gate as JPEGs.
func TestGrokImagesCounted(t *testing.T) {
	w := newWorld(t)
	w.scenario[adapter.Grok] = `{"images":2,"json":{"result":"images/1.jpg and images/2.jpg"}}`
	req := w.req(adapter.Image, "two cats")
	req.Vendor = "grok"
	res, err := w.borrower().Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]
	if r.Status != "done" || r.Made != 2 || len(r.Artifacts) != 2 || len(r.Refused) != 0 {
		t.Fatalf("result %+v", r)
	}
	for i, a := range r.Artifacts {
		dir := filepath.Dir(a.Path)
		if filepath.Base(a.Path) != []string{"1.jpg", "2.jpg"}[i] || filepath.Base(dir) != "images" ||
			!strings.HasPrefix(dir, filepath.Join(w.home, ".grok", "sessions")+string(filepath.Separator)) {
			t.Errorf("artifact %d = %s", i, a.Path)
		}
	}
}
