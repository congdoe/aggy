package brief

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/aggydir"
	"github.com/congdoe/aggy/internal/safefs"
)

// BriefsDir is the folder under .aggy that holds the briefs handed to
// children, one <child-id>.md each.
const BriefsDir = "briefs"

// Path returns where WriteFile puts the brief of childID under dir:
// dir/.aggy/briefs/<childID>.md. childID must be an aggy child id
// (lowercase hex and dashes).
func Path(dir, childID string) (string, error) {
	if childID == "" || len(childID) > 64 || strings.Trim(childID, "0123456789abcdef-") != "" {
		return "", fmt.Errorf("brief: %q is not a child id", childID)
	}
	return filepath.Join(dir, aggydir.Name, BriefsDir, childID+".md"), nil
}

// WriteFile writes text, a brief from Build (already redacted), to
// Path(dir, childID) under aggy's .aggy rule (internal/aggydir): the
// folders are made through descriptors, nothing under .aggy may be a
// symlink, and a brief left there before for the child is replaced, never
// written through. The briefs folder gets a .gitignore of "*", so briefs,
// which hold transcripts, stay out of git (G34-4); the repo's own
// .gitignore and .git are left alone. It returns the file's path. The
// brief is never an argument: the child is told to read this file
// (adapter.BriefPrompt).
func WriteFile(dir, childID, text string) (string, error) {
	if _, err := Path(dir, childID); err != nil {
		return "", err
	}
	d, di, err := aggydir.MkdirAll(dir, 0o700, BriefsDir)
	if err != nil {
		return "", err
	}
	ignore := filepath.Join(d, ".gitignore")
	switch fi, err := safefs.Lstat(ignore); {
	case errors.Is(err, fs.ErrNotExist):
		if err := safefs.WriteFileNoFollowIn(di, ignore, []byte("*\n"), 0o600, nil); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", err
		}
	case err != nil:
		return "", err
	case !fi.Mode().IsRegular():
		return "", fmt.Errorf("%s: %w; aggy will not write through it", ignore, aggydir.ErrLink)
	}
	name := filepath.Join(d, childID+".md")
	var want fs.FileInfo
	switch fi, err := safefs.Lstat(name); {
	case err == nil && !fi.Mode().IsRegular():
		return "", fmt.Errorf("%s: %w; aggy will not write through it", name, aggydir.ErrLink)
	case err == nil:
		want = fi
	case !errors.Is(err, fs.ErrNotExist):
		return "", err
	}
	if err := safefs.WriteFileNoFollowIn(di, name, []byte(text), 0o600, want); err != nil {
		return "", err
	}
	return name, nil
}
