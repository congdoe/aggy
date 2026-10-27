// Package home locates aggy's per-user state directory: $AGGY_HOME when
// set, else ~/.aggy. It holds the index database and the user config.
package home

import (
	"errors"
	"os"
	"path/filepath"
)

// Env names the variable that overrides the state directory, for tests,
// fake CLIs, and users who keep state elsewhere.
const Env = "AGGY_HOME"

// Dir returns the absolute state directory. It does not create it.
func Dir() (string, error) {
	if d := os.Getenv(Env); d != "" {
		return filepath.Abs(d)
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if h == "" {
		return "", errors.New("home directory is not set")
	}
	return filepath.Join(h, ".aggy"), nil
}
