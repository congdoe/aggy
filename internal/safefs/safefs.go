// Package safefs is the only way aggy touches the file system. Every call
// first checks the path against the vendor credential files, which aggy must
// never read, copy, or store, and refuses them with ErrAuthPath.
//
// The check runs on the cleaned absolute path and again after symlinks are
// resolved, so a link to a credential file, or a path through a linked
// directory, is refused too. On macOS, whose default file systems ignore
// case, paths compare case-insensitively.
package safefs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrAuthPath matches, through errors.Is, every refusal of a vendor
// credential path.
var ErrAuthPath = errors.New("refusing to touch a vendor credential file")

// AuthPathError reports a refused path. errors.Is(err, ErrAuthPath) is true
// for it.
type AuthPathError struct {
	Op   string // the safefs function, such as "ReadFile"
	Path string // the path as the caller gave it
}

func (e *AuthPathError) Error() string {
	return fmt.Sprintf("safefs: %s %s: %v", e.Op, e.Path, ErrAuthPath)
}

// Is reports whether target is ErrAuthPath.
func (e *AuthPathError) Is(target error) bool { return target == ErrAuthPath }

// foldCase is true where the usual file system ignores case. It is a
// variable so tests can exercise both behaviors on any OS.
var foldCase = runtime.GOOS == "darwin"

// authPaths returns the credential files aggy refuses, as absolute paths.
// The defaults are always included. CLAUDE_CONFIG_DIR, CODEX_HOME, and
// GROK_HOME relocate each vendor's config directory, so the file under a
// relocated directory is refused as well. Each name was checked against the
// vendor's own binary or documentation.
func authPaths() []string {
	var paths []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths,
			filepath.Join(home, ".claude", ".credentials.json"),
			filepath.Join(home, ".codex", "auth.json"),
			filepath.Join(home, ".grok", "auth.json"),
		)
	}
	for _, rel := range []struct{ env, file string }{
		{"CLAUDE_CONFIG_DIR", ".credentials.json"},
		{"CODEX_HOME", "auth.json"},
		{"GROK_HOME", "auth.json"},
	} {
		if dir := os.Getenv(rel.env); dir != "" {
			if abs, err := filepath.Abs(dir); err == nil {
				paths = append(paths, filepath.Join(abs, rel.file))
			}
		}
	}
	return paths
}

// Check returns an *AuthPathError when name is, or resolves to, a vendor
// credential file. It never opens or stats the credential files themselves:
// only their parent directories are resolved.
func Check(name string) error { return check("Check", name) }

func check(op, name string) error {
	if _, err := resolve(op, name); err != nil {
		return err
	}
	return nil
}

// resolve checks name and returns its absolute path with symlinks resolved
// as far as the path exists.
func resolve(op, name string) (string, error) {
	abs, err := filepath.Abs(name)
	if err != nil {
		return "", &fs.PathError{Op: op, Path: name, Err: err}
	}
	// Each credential file is compared as given and with its directory
	// resolved, so a linked vendor directory is covered.
	var denied []string
	for _, auth := range authPaths() {
		denied = append(denied, auth)
		if dir, err := evalExisting(filepath.Dir(auth)); err == nil {
			denied = append(denied, filepath.Join(dir, filepath.Base(auth)))
		}
	}
	// The lexical check runs first, so a plain credential path is refused
	// before anything on disk is looked at.
	if matchAny(abs, denied) {
		return "", &AuthPathError{Op: op, Path: name}
	}
	real, err := evalExisting(abs)
	if err != nil {
		return "", &fs.PathError{Op: op, Path: name, Err: err}
	}
	if matchAny(real, denied) {
		return "", &AuthPathError{Op: op, Path: name}
	}
	return real, nil
}

func matchAny(path string, denied []string) bool {
	for _, d := range denied {
		if samePath(path, d) {
			return true
		}
	}
	return false
}

// evalExisting resolves symlinks in the longest existing prefix of path and
// appends the rest unchanged, so a file that does not exist yet still gets
// its directory resolved.
func evalExisting(path string) (string, error) {
	path = filepath.Clean(path)
	var rest []string
	for {
		real, err := filepath.EvalSymlinks(path)
		if err == nil {
			return filepath.Join(append([]string{real}, rest...)...), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Join(append([]string{path}, rest...)...), nil
		}
		rest = append([]string{filepath.Base(path)}, rest...)
		path = parent
	}
}

func samePath(a, b string) bool {
	if foldCase {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Open opens name for reading.
func Open(name string) (*os.File, error) {
	if err := check("Open", name); err != nil {
		return nil, err
	}
	return os.Open(name)
}

// OpenFile is os.OpenFile behind the credential check.
func OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	if err := check("OpenFile", name); err != nil {
		return nil, err
	}
	return os.OpenFile(name, flag, perm)
}

// ReadFile reads the whole file.
func ReadFile(name string) ([]byte, error) {
	if err := check("ReadFile", name); err != nil {
		return nil, err
	}
	return os.ReadFile(name)
}

// WriteFile writes data atomically: it writes a temp file in the target's
// directory, syncs it, and renames it over the target. An existing file
// keeps its mode and perm is ignored; a new file gets perm. When name is a
// symlink, the file it points to is replaced and the link is kept.
func WriteFile(name string, data []byte, perm fs.FileMode) error {
	real, err := resolve("WriteFile", name)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(real); err == nil {
		if !fi.Mode().IsRegular() {
			return &fs.PathError{Op: "WriteFile", Path: name, Err: errors.New("not a regular file")}
		}
		perm = fi.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(real), "."+filepath.Base(real)+".aggy-*")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), real); err != nil {
		return err
	}
	ok = true
	return nil
}

// Stat is os.Stat behind the credential check.
func Stat(name string) (fs.FileInfo, error) {
	if err := check("Stat", name); err != nil {
		return nil, err
	}
	return os.Stat(name)
}

// Lstat is os.Lstat behind the credential check.
func Lstat(name string) (fs.FileInfo, error) {
	if err := check("Lstat", name); err != nil {
		return nil, err
	}
	return os.Lstat(name)
}

// ReadDir is os.ReadDir behind the credential check.
func ReadDir(name string) ([]fs.DirEntry, error) {
	if err := check("ReadDir", name); err != nil {
		return nil, err
	}
	return os.ReadDir(name)
}

// MkdirAll is os.MkdirAll behind the credential check.
func MkdirAll(name string, perm fs.FileMode) error {
	if err := check("MkdirAll", name); err != nil {
		return err
	}
	return os.MkdirAll(name, perm)
}

// Remove is os.Remove behind the credential check.
func Remove(name string) error {
	if err := check("Remove", name); err != nil {
		return err
	}
	return os.Remove(name)
}

// Chmod is os.Chmod behind the credential check.
func Chmod(name string, mode fs.FileMode) error {
	if err := check("Chmod", name); err != nil {
		return err
	}
	return os.Chmod(name, mode)
}
