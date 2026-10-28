package borrow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
	"github.com/congdoe/aggy/internal/safefs"
)

// Media is the file arguments of aggy image and aggy video. The child
// never writes the repo: aggy copies what it made to Out.
type Media struct {
	// Out is the absolute path the result is copied to, inside the repo.
	Out string
	// Refs are absolute paths of reference images (image only). aggy
	// copies each, through the artifact gate, into the run's own dir and
	// gives the CLI the copy.
	Refs []string
	// N is the number of variants asked for; 0 means 1.
	N int
	// Resume is the vendor session an --edit continues, from the
	// sidecar of the image edited; Request.Vendor is that vendor.
	Resume string
}

// MaxImages caps the variants one image borrow asks for, and the files
// one borrow keeps. Image turns use a plan's limits several times faster
// than text.
const MaxImages = 4

// MaxRefs caps the reference images of one borrow.
const MaxRefs = 4

// outExts are the --out extensions each kind takes.
var outExts = map[adapter.Kind][]string{
	adapter.Image: {".png", ".jpg", ".jpeg"},
	adapter.Video: {".mp4"},
}

// validate checks m for a borrow of req's kind.
func (m *Media) validate(req Request) error {
	exts, ok := outExts[req.Kind]
	if !ok {
		return fmt.Errorf("--out is for image and video borrows, not %s", req.Kind)
	}
	if !filepath.IsAbs(m.Out) || strings.ContainsAny(m.Out, "\n\r") {
		return fmt.Errorf("--out %q must be absolute and on one line", m.Out)
	}
	out := filepath.Clean(m.Out)
	// The repo's own .git and .aggy are aggy's and git's, not assets.
	rel, _ := filepath.Rel(req.Repo, out)
	if !within(out, req.Repo) || out == req.Repo || reserved(rel) != "" {
		return fmt.Errorf("--out %s must be a file inside %s, outside its .git and .aggy", out, req.Repo)
	}
	if ext := strings.ToLower(filepath.Ext(out)); !slices.Contains(exts, ext) {
		return fmt.Errorf("--out %s must end in %s", out, strings.Join(exts, " or "))
	}
	switch {
	case m.N < 0 || m.N > MaxImages:
		return fmt.Errorf("--n must be 1 to %d", MaxImages)
	case req.Kind == adapter.Video && m.N > 1:
		return errors.New("--n is for images")
	case req.Kind == adapter.Video && len(m.Refs) > 0:
		return errors.New("--ref is for images")
	case len(m.Refs) > MaxRefs:
		return fmt.Errorf("at most %d --ref images", MaxRefs)
	}
	for _, r := range m.Refs {
		if !filepath.IsAbs(r) || strings.ContainsAny(r, "\n\r") {
			return fmt.Errorf("--ref %q must be absolute and on one line", r)
		}
	}
	if m.Resume != "" {
		v, err := adapter.ParseVendor(req.Vendor)
		if err != nil || req.Kind != adapter.Image {
			return errors.New("an edit resumes an image session on the vendor that made it")
		}
		if err := adapter.ValidSessionID(v, m.Resume); err != nil {
			return err
		}
	}
	return nil
}

// constraints are the lines m adds to the child's prompt.
func (m *Media) constraints() []string {
	var out []string
	if m.N > 1 {
		out = append(out, fmt.Sprintf("Make %d separate images, variants of the same brief, and no more.", m.N))
	} else {
		out = append(out, "Make one image or video, and no more.")
	}
	if len(m.Refs) > 0 {
		out = append(out, "The attached images are references for style and content.")
	}
	if m.Resume != "" {
		out = append(out, "Edit the image you made earlier in this session as the task says; make a new image file.")
	}
	return out
}

// stageRefs copies each reference image into run, through the artifact
// gate with no time limit (a reference is the user's own file, made any
// time), and returns the copies' paths. The CLI reads the copy, so a
// reference swapped for a link to a secret after the check is never
// sent; and only a real PNG or JPEG with one link is.
func stageRefs(refs []string, run string) ([]string, error) {
	var out []string
	for i, r := range refs {
		m, err := readArtifact(adapter.Artifact{Path: r, Kind: "image"}, time.Time{})
		if err != nil {
			return nil, fmt.Errorf("--ref %w", err)
		}
		p := filepath.Join(run, fmt.Sprintf("ref-%d%s", i+1, m.Ext()))
		if err := safefs.WriteFile(p, m.Data, 0o600); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// Sidecar is what aggy records about a generated asset, in
// <repo>/.aggy/assets/<path>.json: enough for the on-screen agent to know
// where it came from, and for --edit to resume the same child.
type Sidecar struct {
	Prompt    string    `json:"prompt"`
	Kind      string    `json:"kind"`
	Vendor    string    `json:"vendor"`
	Model     string    `json:"model,omitempty"`
	SessionID string    `json:"session_id"`
	Borrow    string    `json:"borrow"`
	Created   time.Time `json:"created"`
}

// AssetsDir is where sidecars go, inside the repo.
const AssetsDir = ".aggy/assets"

// SidecarPath is the sidecar of out, a file inside repo.
func SidecarPath(repo, out string) (string, error) {
	rel, err := filepath.Rel(repo, out)
	if err != nil || !within(out, repo) || out == repo {
		return "", fmt.Errorf("%s is not inside %s", out, repo)
	}
	return filepath.Join(repo, filepath.FromSlash(AssetsDir), rel+".json"), nil
}

// checkResume lets an --edit resume only a session aggy's own image or
// video borrow recorded on that vendor. A sidecar sits in the repo, so a
// cloned repo could name any session, such as one of the user's own
// Codex threads, to be resumed headless.
func (b *Borrower) checkResume(ctx context.Context, req Request) error {
	if req.Media == nil || req.Media.Resume == "" {
		return nil
	}
	ok, err := b.Store.MediaSession(ctx, req.Vendor, req.Media.Resume)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("--edit: %s session %s was not made by an aggy image or video borrow on this machine, so aggy will not resume it", req.Vendor, req.Media.Resume)
	}
	return nil
}

// MaxSidecar caps a sidecar file read for --edit.
const MaxSidecar = 64 << 10

// ReadSidecar reads the sidecar of out, a file inside repo, for --edit.
// The sidecar sits in the repo, so it is checked like input: a known
// vendor and a valid session id for it, and an image.
func ReadSidecar(repo, out string) (Sidecar, error) {
	p, err := SidecarPath(repo, out)
	if err != nil {
		return Sidecar{}, err
	}
	data, err := safefs.ReadFileLimit(p, MaxSidecar)
	if err != nil {
		return Sidecar{}, fmt.Errorf("%s has no sidecar aggy wrote (%w)", out, err)
	}
	var sc Sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return Sidecar{}, fmt.Errorf("sidecar %s: %w", p, err)
	}
	v, err := adapter.ParseVendor(sc.Vendor)
	if err != nil {
		return Sidecar{}, fmt.Errorf("sidecar %s: %w", p, err)
	}
	if err := adapter.ValidSessionID(v, sc.SessionID); err != nil {
		return Sidecar{}, fmt.Errorf("sidecar %s: %w", p, err)
	}
	if sc.Kind != string(adapter.Image) {
		return Sidecar{}, fmt.Errorf("sidecar %s is for a %q, and only images are edited", p, sc.Kind)
	}
	return sc, nil
}

// outPath is the k-th name for out: out, then out's name with -2, -3,
// and so on. When out's extension does not fit what the vendor made (a
// JPEG for hero.png), the extension is changed to fit, and the report
// shows the path written.
func outPath(out string, k int, m media) string {
	ext := filepath.Ext(out)
	stem := strings.TrimSuffix(out, ext)
	switch e := strings.ToLower(ext); {
	case m.Type == JPEG && (e == ".jpg" || e == ".jpeg"), m.Type == PNG && e == ".png", m.Type == MP4 && e == ".mp4":
	default:
		ext = m.Ext()
	}
	if k > 0 {
		stem += fmt.Sprintf("-%d", k+1)
	}
	return stem + ext
}

// maxNames bounds the search for a free variant name.
const maxNames = 100

// deliver copies the kept files into the repo and writes a sidecar for
// each. The child never writes the repo; aggy does, here. Only --out
// itself, as the caller named it, may replace a file (a regular one,
// never through a symlink); every other name (a variant, or --out with
// its extension changed) is created exclusively, moving to the next free
// name when one exists. It returns the paths written.
func deliver(req Request, res Result, kept []media, sessionID string, created time.Time) ([]adapter.Artifact, error) {
	if len(kept) == 0 {
		return nil, errors.New("the child left no file that passed the artifact gate")
	}
	var out []adapter.Artifact
	k := 0
	for _, m := range kept {
		var p string
		for ; ; k++ {
			if k == maxNames {
				return out, fmt.Errorf("no free name for %s after %d tries", req.Media.Out, maxNames)
			}
			p = outPath(req.Media.Out, k, m)
			err := writeIn(req.Repo, p, m.Data, 0o755, 0o644, false, p == filepath.Clean(req.Media.Out))
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			if err != nil {
				return out, err
			}
			k++
			break
		}
		out = append(out, adapter.Artifact{Path: p, Kind: m.Kind})
		sc, err := SidecarPath(req.Repo, p)
		if err != nil {
			return out, err
		}
		data, err := json.MarshalIndent(Sidecar{
			Prompt: req.Task, Kind: string(req.Kind), Vendor: string(res.Vendor), Model: res.Model,
			SessionID: sessionID, Borrow: res.ID, Created: created.UTC(),
		}, "", "  ")
		if err != nil {
			return out, err
		}
		if err := writeIn(req.Repo, sc, append(data, '\n'), 0o700, 0o600, true, true); err != nil {
			return out, err
		}
	}
	return out, nil
}

// reserved reports whether rel, a path relative to the repo, is inside
// its .git or .aggy, compared case-insensitively (APFS and NTFS fold
// case, so .GIT is .git there).
func reserved(rel string) string {
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	if f := strings.ToLower(first); f == ".git" || f == ".aggy" {
		return f
	}
	return ""
}

// writeIn writes data to p, a path inside repo, through safefs. It never
// writes through a symlink or out of the repo:
//
//   - it finds p's deepest existing ancestor dir, resolves it, and checks
//     that it is inside the resolved repo, and inside its .aggy only when
//     aggyDir (sidecars) and its .git never; then it makes only the
//     missing dirs, one at a time and without following (safefs.Mkdir);
//   - it takes that dir's identity, and the write lands only in it
//     (safefs.WriteFileNoFollowIn), so a dir swapped after the check
//     fails rather than escapes;
//   - with replace, an existing regular file at p is replaced (the name,
//     never through a link); anything else there is refused. Without
//     replace, p must not exist: the error then matches fs.ErrExist.
func writeIn(repo, p string, data []byte, dirPerm, perm fs.FileMode, aggyDir, replace bool) error {
	realRepo := resolve(repo)
	// inside checks a dir on the way: in the repo, never in .git, and in
	// .aggy only for sidecars (whose way runs through the repo root).
	inside := func(real string, final bool) error {
		rel, err := filepath.Rel(realRepo, real)
		if err != nil || !within(real, realRepo) {
			return fmt.Errorf("%s resolves to %s, outside %s", filepath.Dir(p), real, repo)
		}
		r := reserved(rel)
		bad := r == ".git" || (!aggyDir && r == ".aggy") || (aggyDir && r != ".aggy" && (final || real != realRepo))
		if bad {
			return fmt.Errorf("%s resolves to %s, which is not where aggy writes this file", filepath.Dir(p), real)
		}
		return nil
	}
	var missing []string
	cur := filepath.Dir(p)
	for {
		_, err := safefs.Lstat(cur)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if filepath.Dir(cur) == cur {
			return fmt.Errorf("no part of %s exists", p)
		}
		missing = append([]string{filepath.Base(cur)}, missing...)
		cur = filepath.Dir(cur)
	}
	real, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return err
	}
	if err := inside(real, len(missing) == 0); err != nil {
		return err
	}
	for i, m := range missing {
		real = filepath.Join(real, m)
		if err := inside(real, i == len(missing)-1); err != nil {
			return err
		}
		if err := safefs.Mkdir(real, dirPerm); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
	}
	dir, err := safefs.Lstat(real)
	if err != nil {
		return err
	}
	// No element of real may have become a symlink while the dirs were
	// made: its identity is what the write is pinned to.
	if again, err := filepath.EvalSymlinks(real); err != nil || again != real || !dir.IsDir() {
		return fmt.Errorf("%s changed while aggy made it", real)
	}
	name := filepath.Join(real, filepath.Base(p))
	var want fs.FileInfo
	if replace {
		fi, err := safefs.Lstat(name)
		switch {
		case err == nil && !fi.Mode().IsRegular():
			return fmt.Errorf("%s is not a regular file (a symlink?); aggy will not write through it", p)
		case err == nil:
			want = fi
		case !errors.Is(err, fs.ErrNotExist):
			return err
		}
	}
	return safefs.WriteFileNoFollowIn(dir, name, data, perm, want)
}
