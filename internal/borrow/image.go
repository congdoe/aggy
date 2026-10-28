package borrow

import (
	"errors"
	"fmt"
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
}

// MaxImages caps the variants one image borrow asks for. Image turns use
// a plan's limits several times faster than text.
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
	if !within(out, req.Repo) || out == req.Repo || within(out, filepath.Join(req.Repo, ".git")) || within(out, filepath.Join(req.Repo, ".aggy")) {
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
