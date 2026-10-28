package probe

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/congdoe/aggy/internal/safefs"
)

// ErrNotFound means a vendor binary is not on aggy's PATH.
var ErrNotFound = errors.New("executable file not found in $PATH")

// cleanPath drops empty, ".", and relative PATH entries, which would make
// a lookup depend on the working directory. internal/execrun applies the
// same rule to the PATH a CLI inherits.
func cleanPath(p string) string {
	var keep []string
	for _, d := range filepath.SplitList(p) {
		if d != "" && filepath.IsAbs(d) {
			keep = append(keep, d)
		}
	}
	return strings.Join(keep, string(filepath.ListSeparator))
}

// LookPath finds the vendor binary name on aggy's PATH, using only its
// absolute entries, and returns a clean absolute path for the runner
// (internal/execrun). It starts nothing. A missing binary matches
// ErrNotFound.
func LookPath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("probe: %q is not a bare program name", name)
	}
	for _, dir := range filepath.SplitList(cleanPath(os.Getenv("PATH"))) {
		p := filepath.Join(dir, name)
		fi, err := safefs.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		return filepath.Clean(p), nil
	}
	return "", fmt.Errorf("probe: %s: %w", name, ErrNotFound)
}
