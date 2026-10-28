// Package home locates aggy's per-user state directory: $AGGY_HOME when
// set, else ~/.aggy. It holds the index database and the user config.
package home

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/congdoe/aggy/internal/safefs"
)

// Env names the variable that overrides the state directory, for tests,
// fake CLIs, and users who keep state elsewhere.
const Env = "AGGY_HOME"

// Dir returns the absolute state directory. It does not create it.
//
// A relative AGGY_HOME is refused: it would name a different directory
// in every working directory, and so a different index and backups.
//
// The state directory itself may be a symlink, as dotfile setups make
// it: Dir then returns the directory it points to, so everything below
// is a real path, and safefs keeps refusing symlinks inside it (such as
// a linked config.toml). Only the root is resolved, and only when it is
// a symlink.
func Dir() (string, error) {
	d := os.Getenv(Env)
	if d != "" {
		if !filepath.IsAbs(d) {
			return "", fmt.Errorf("%s=%q is not an absolute path", Env, d)
		}
		d = filepath.Clean(d)
	} else {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if h == "" {
			return "", errors.New("home directory is not set")
		}
		d = filepath.Join(h, ".aggy")
	}
	return resolveRoot(d)
}

// lstat is safefs.Lstat: home reads metadata only, through safefs.
var lstat = safefs.Lstat

// resolveRoot returns the target of d when d is a symlink, else d.
func resolveRoot(d string) (string, error) {
	fi, err := lstat(d)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return d, nil
	}
	r, err := filepath.EvalSymlinks(d)
	if err != nil {
		return "", fmt.Errorf("the state directory %s is a symlink that cannot be resolved: %w", d, err)
	}
	return r, nil
}
