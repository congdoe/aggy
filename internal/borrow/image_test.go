package borrow

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/adapter"
)

var (
	pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
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
		{"edit with no vendor", adapter.Image, Media{Out: "/r/a.png", Resume: "5f0c6a2e-9b1d-4e7a-8c3f-2d4b6a8e0c1f"}, "on the vendor that made it"},
		{"edit a video", adapter.Video, Media{Out: "/r/a.mp4", Resume: "5f0c6a2e-9b1d-4e7a-8c3f-2d4b6a8e0c1f"}, "on the vendor that made it"},
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
	if !strings.Contains(rec.Stdin+rec.Prompt, "Make 3 separate images") || !strings.Contains(rec.Stdin+rec.Prompt, "references") {
		t.Errorf("prompt lacks the variants or refs:\n%s", rec.Stdin+rec.Prompt)
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

// TestDeliver (T6634): aggy copies what the child made to --out, never
// the child, and writes a sidecar with the prompt, vendor, model, and
// child session id.
func TestDeliver(t *testing.T) {
	tests := []struct {
		name     string
		vendor   adapter.Vendor
		scenario string
		out      string
		n        int
		link     bool // public is a symlink out of the repo
		status   string
		files    []string // written, relative to the repo
		detail   string
	}{
		{"codex png", adapter.Codex, `{"images":1}`, "public/hero.png", 0, false, "done", []string{"public/hero.png"}, ""},
		{"codex variants", adapter.Codex, `{"images":2}`, "public/hero.png", 2, false, "done", []string{"public/hero.png", "public/hero-2.png"}, ""},
		{"grok jpeg for a .png", adapter.Grok, `{"images":1,"json":{"result":"images/1.jpg"}}`, "hero.png", 0, false, "done", []string{"hero.jpg"}, ""},
		{"grok jpeg kept as .jpeg", adapter.Grok, `{"images":1,"json":{"result":"images/1.jpg"}}`, "hero.jpeg", 0, false, "done", []string{"hero.jpeg"}, ""},
		{"nothing made", adapter.Codex, `{}`, "hero.png", 0, false, "failed", nil, "left no file that passed"},
		{"symlinked folder", adapter.Codex, `{"images":1}`, "public/hero.png", 0, true, "failed", nil, "outside"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.scenario[tt.vendor] = tt.scenario
			if tt.link {
				if err := os.Symlink(t.TempDir(), filepath.Join(w.repo, "public")); err != nil {
					t.Fatal(err)
				}
			}
			req := w.req(adapter.Image, "a hero image")
			req.Vendor = string(tt.vendor)
			req.Media = &Media{Out: filepath.Join(w.repo, tt.out), N: tt.n}
			res, err := w.borrower().Run(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			r := res[0]
			if r.Status != tt.status || !strings.Contains(r.Detail, tt.detail) || len(r.Artifacts) != len(tt.files) {
				t.Fatalf("result %+v", r)
			}
			for i, f := range tt.files {
				p := filepath.Join(w.repo, f)
				if r.Artifacts[i].Path != p {
					t.Errorf("artifact %d = %s, want %s", i, r.Artifacts[i].Path, p)
				}
				data, err := os.ReadFile(p)
				if err != nil || sniff(data) == "" {
					t.Errorf("%s: %v, not an image", f, err)
				}
				b, err := os.ReadFile(filepath.Join(w.repo, ".aggy", "assets", f+".json"))
				if err != nil {
					t.Fatal(err)
				}
				var sc Sidecar
				if err := json.Unmarshal(b, &sc); err != nil {
					t.Fatal(err)
				}
				if sc.Prompt != "a hero image" || sc.Vendor != string(tt.vendor) || sc.Kind != "image" || sc.Borrow != r.ID ||
					adapter.ValidSessionID(tt.vendor, sc.SessionID) != nil {
					t.Errorf("sidecar %+v", sc)
				}
			}
		})
	}
}

// TestEditResumes (T6635): --edit resumes the child session that made
// the image, on its vendor, read from the sidecar, and writes over it.
func TestEditResumes(t *testing.T) {
	tests := []struct {
		vendor   adapter.Vendor
		scenario string
		want     []string // in the resumed run's args
	}{
		{adapter.Codex, `{"images":1}`, []string{"resume"}},
		{adapter.Grok, `{"images":1,"json":{"result":"images/1.jpg"}}`, []string{"--resume", "image_gen,image_edit"}},
	}
	for _, tt := range tests {
		t.Run(string(tt.vendor), func(t *testing.T) {
			w := newWorld(t)
			w.cfg.Media.GrokFileTools = true // image_edit, on Grok
			w.scenario[tt.vendor] = tt.scenario
			out := filepath.Join(w.repo, "hero.jpg")
			req := w.req(adapter.Image, "a hero image")
			req.Vendor = string(tt.vendor)
			req.Media = &Media{Out: out}
			first, err := w.borrower().Run(context.Background(), req)
			if err != nil || first[0].Status != "done" {
				t.Fatalf("first run %+v, %v", first, err)
			}
			out = first[0].Artifacts[0].Path
			sc, err := ReadSidecar(w.repo, out)
			if err != nil {
				t.Fatal(err)
			}
			// The vendor names new files afresh; the fake reuses 1.*, so
			// clear the first run's file.
			if err := os.Remove(firstSource(t, w, tt.vendor)); err != nil {
				t.Fatal(err)
			}
			req = w.req(adapter.Image, "darker background")
			req.Vendor = sc.Vendor
			req.Media = &Media{Out: out, Resume: sc.SessionID}
			res, err := w.borrower().Run(context.Background(), req)
			if err != nil || res[0].Status != "done" || len(res[0].Artifacts) != 1 || res[0].Artifacts[0].Path != out {
				t.Fatalf("edit %+v, %v", res, err)
			}
			recs := w.records()
			rec := recs[len(recs)-1]
			args := strings.Join(rec.Args, " ")
			if rec.SessionID != sc.SessionID || !strings.Contains(args, sc.SessionID) || !strings.Contains(rec.Stdin+rec.Prompt, "Edit the image you made earlier") {
				t.Errorf("resumed run %+v", rec)
			}
			for _, x := range tt.want {
				if !strings.Contains(args, x) {
					t.Errorf("args %q lack %q", args, x)
				}
			}
			after, err := ReadSidecar(w.repo, out)
			if err != nil || after.SessionID != sc.SessionID || after.Prompt != "darker background" || after.Borrow == sc.Borrow {
				t.Errorf("sidecar after edit %+v, %v", after, err)
			}
		})
	}
}

// firstSource is the one file the vendor stored for the first run.
func firstSource(t *testing.T, w *world, v adapter.Vendor) string {
	t.Helper()
	pattern := filepath.Join(w.home, ".codex", "generated_images", "*", "1.png")
	if v == adapter.Grok {
		pattern = filepath.Join(w.home, ".grok", "sessions", "*", "*", "images", "1.jpg")
	}
	m, err := filepath.Glob(pattern)
	if err != nil || len(m) != 1 {
		t.Fatalf("glob %s = %v, %v", pattern, m, err)
	}
	return m[0]
}

func TestReadSidecar(t *testing.T) {
	repo := t.TempDir()
	out := filepath.Join(repo, "a.png")
	dir := filepath.Join(repo, ".aggy", "assets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "5f0c6a2e-9b1d-4e7a-8c3f-2d4b6a8e0c1f"
	tests := []struct {
		name, body, want string
	}{
		{"ok", `{"kind":"image","vendor":"grok","session_id":"` + id + `"}`, ""},
		{"flag id", `{"kind":"image","vendor":"grok","session_id":"--yolo"}`, "session id"},
		{"unknown vendor", `{"kind":"image","vendor":"gemini","session_id":"` + id + `"}`, "unknown vendor"},
		{"video", `{"kind":"video","vendor":"grok","session_id":"` + id + `"}`, "only images are edited"},
		{"garbage", `{`, "sidecar"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(dir, "a.png.json"), []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := ReadSidecar(repo, out)
			if (err == nil) != (tt.want == "") || (err != nil && !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("ReadSidecar = %v, want %q", err, tt.want)
			}
		})
	}
	if _, err := ReadSidecar(repo, filepath.Join(repo, "none.png")); err == nil || !strings.Contains(err.Error(), "no sidecar") {
		t.Errorf("missing sidecar = %v", err)
	}
}

// TestImageCap (T6636): a vendor that makes more than the cap has every
// file counted, and only MaxImages kept and copied.
func TestImageCap(t *testing.T) {
	w := newWorld(t)
	w.scenario[adapter.Codex] = `{"images":6}`
	req := w.req(adapter.Image, "cats")
	req.Vendor = "codex"
	req.Media = &Media{Out: filepath.Join(w.repo, "cat.png"), N: 2}
	res, err := w.borrower().Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]
	if r.Status != "done" || r.Made != 6 || len(r.Artifacts) != MaxImages || len(r.Refused) != 1 ||
		!strings.Contains(r.Refused[0], "2 more not kept: the cap is 4") {
		t.Fatalf("result %+v", r)
	}
	if _, err := os.Stat(filepath.Join(w.repo, "cat-5.png")); err == nil {
		t.Error("a fifth image was copied")
	}
	row, _ := w.store.GetBorrow(context.Background(), r.ID)
	if row.ImageCount != 6 {
		t.Errorf("image count %d", row.ImageCount)
	}
}

// TestVideo (T6637): a video borrow runs on Grok with image_gen and
// image_to_video, and keeps the MP4 from the session's videos folder,
// not the still it made on the way.
func TestVideo(t *testing.T) {
	w := newWorld(t)
	w.cfg.Media.GrokFileTools = true
	w.scenario[adapter.Grok] = `{"images":1,"videos":1,"json":{"result":"videos/1.mp4"}}`
	req := w.req(adapter.Video, "a logo reveal")
	out := filepath.Join(w.repo, "intro.mp4")
	req.Media = &Media{Out: out}
	res, err := w.borrower().Run(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	r := res[0]
	if r.Vendor != adapter.Grok || r.Status != "done" || r.Made != 1 || len(r.Artifacts) != 1 || r.Artifacts[0] != (adapter.Artifact{Path: out, Kind: "video"}) {
		t.Fatalf("result %+v", r)
	}
	data, err := os.ReadFile(out)
	if err != nil || sniff(data) != MP4 {
		t.Errorf("intro.mp4: %v, %q", err, sniff(data))
	}
	sc, err := os.ReadFile(filepath.Join(w.repo, ".aggy", "assets", "intro.mp4.json"))
	if err != nil || !strings.Contains(string(sc), `"kind": "video"`) {
		t.Errorf("sidecar %s, %v", sc, err)
	}
	recs := w.records()
	if args := strings.Join(recs[len(recs)-1].Args, " "); !strings.Contains(args, "--tools image_gen,image_to_video") {
		t.Errorf("args %q", args)
	}
}

// TestFileToolsOffByDefault (B15-2): without the user's opt-in, a video
// borrow and a Grok image edit start nothing, and the refusal names the
// config key and the upload risk.
func TestFileToolsOffByDefault(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(map[bool]string{false: "video", true: "grok edit"}[edit], func(t *testing.T) {
			w := newWorld(t)
			w.scenario[adapter.Grok] = `{"images":1,"json":{"result":"images/1.jpg"}}`
			req := w.req(adapter.Video, "a thing")
			req.Vendor = "grok"
			req.Media = &Media{Out: filepath.Join(w.repo, "intro.mp4")}
			if edit {
				// A new image needs no opt-in; its edit does.
				req.Kind, req.Media.Out = adapter.Image, filepath.Join(w.repo, "hero.jpg")
				first, err := w.borrower().Run(context.Background(), req)
				if err != nil || first[0].Status != "done" {
					t.Fatalf("first %+v, %v", first, err)
				}
				sc, err := ReadSidecar(w.repo, req.Media.Out)
				if err != nil {
					t.Fatal(err)
				}
				req.Media = &Media{Out: req.Media.Out, Resume: sc.SessionID}
			}
			runs := len(w.records())
			_, err := w.borrower().Run(context.Background(), req)
			if err == nil || !strings.Contains(err.Error(), "grok_file_tools = true under [media]") || !strings.Contains(err.Error(), "uploads to xAI") {
				t.Fatalf("err = %v", err)
			}
			if len(w.records()) != runs {
				t.Error("a child ran")
			}
		})
	}
}
