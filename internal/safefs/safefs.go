// Package safefs is the only way aggy touches the file system. Every call
// refuses the vendor credential files, which aggy must never read, copy, or
// store, with an error that matches ErrAuthPath. The refused files are:
//
//   - Claude: ~/.claude/.credentials.json, or under $CLAUDE_CONFIG_DIR
//   - Codex: ~/.codex/auth.json and ~/.codex/.credentials.json (the
//     file-backed MCP OAuth store), or under $CODEX_HOME
//   - Grok: ~/.grok/auth.json and ~/.grok/mcp_credentials.json, or under
//     $GROK_HOME
//
// Two checks run on every call:
//
//  1. A path check. The path is resolved physically, so a ".." after a
//     symlink means what the kernel takes it to mean, and the resolved path
//     is compared with the credential paths (case-insensitively on macOS).
//     The operation then runs on that resolved path, never on the raw name.
//  2. An identity check. Each credential file that exists is identified by
//     its device and inode (metadata only; its contents are never read).
//     Reads go through an opened descriptor whose identity is checked
//     before any byte is returned, so a symlink swapped in after the path
//     check, a hard link, or a name spelled in another Unicode
//     normalization form is still refused. Stat, Lstat, Chmod, Remove, and
//     WriteFile compare the target's identity before acting.
//
// A credential file that does not exist has nothing to protect and matches
// only by path.
//
// safefs fails closed. When no absolute home directory can be found, or
// CLAUDE_CONFIG_DIR, CODEX_HOME, or GROK_HOME is set to a relative path,
// every call returns an error matching ErrLocate.
package safefs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
)

// ErrAuthPath matches, through errors.Is, every refusal of a vendor
// credential file.
var ErrAuthPath = errors.New("refusing to touch a vendor credential file")

// ErrLocate means the credential files cannot be located, so safefs refuses
// all access rather than guess.
var ErrLocate = errors.New("cannot locate vendor credential files")

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

// lookupUserHome is the fallback when $HOME is unset or relative. Tests
// replace it so they never reach the real home directory.
var lookupUserHome = func() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

// testHookBeforeOpen, when set by a test, runs between the path checks and
// the open, so a test can swap the file in exactly that window.
var testHookBeforeOpen func()

// credDirs lists each vendor's config directory: the default under home,
// the variable that relocates it, and the credential files inside. Each
// variable name was checked against the vendor's own binary or docs.
var credDirs = []struct {
	env, dir string
	files    []string
}{
	{"CLAUDE_CONFIG_DIR", ".claude", []string{".credentials.json"}},
	{"CODEX_HOME", ".codex", []string{"auth.json", ".credentials.json"}},
	{"GROK_HOME", ".grok", []string{"auth.json", "mcp_credentials.json"}},
}

// homeDir returns an absolute home directory: $HOME, else the user
// database. A relative value is never used.
func homeDir() (string, error) {
	if h := os.Getenv("HOME"); h != "" && filepath.IsAbs(h) {
		return filepath.Clean(h), nil
	}
	h, err := lookupUserHome()
	if err == nil && h != "" && filepath.IsAbs(h) {
		return filepath.Clean(h), nil
	}
	return "", fmt.Errorf("%w: no absolute home directory", ErrLocate)
}

// guard is the deny list for one call. It is rebuilt each call because the
// environment can change.
type guard struct {
	paths []string      // absolute credential paths, as given and with the directory resolved
	ids   []fs.FileInfo // identities of the credential files that exist
}

func loadGuard() (*guard, error) {
	home, err := homeDir()
	if err != nil {
		return nil, err
	}
	var creds []string
	for _, c := range credDirs {
		dirs := []string{filepath.Join(home, c.dir)}
		if v := os.Getenv(c.env); v != "" {
			if !filepath.IsAbs(v) {
				return nil, fmt.Errorf("%w: %s=%q is not an absolute path", ErrLocate, c.env, v)
			}
			dirs = append(dirs, filepath.Clean(v))
		}
		for _, d := range dirs {
			for _, f := range c.files {
				creds = append(creds, filepath.Join(d, f))
			}
		}
	}
	g := &guard{}
	for _, p := range creds {
		g.paths = append(g.paths, p)
		if d, err := resolveExisting(filepath.Dir(p)); err == nil {
			g.paths = append(g.paths, filepath.Join(d, filepath.Base(p)))
		}
		// Metadata only: these calls never read the file's contents.
		if fi, err := os.Lstat(p); err == nil {
			g.ids = append(g.ids, fi)
		}
		if fi, err := os.Stat(p); err == nil {
			g.ids = append(g.ids, fi)
		}
	}
	return g, nil
}

// pathMatch reports whether path names a credential file.
func (g *guard) pathMatch(path string) bool {
	for _, d := range g.paths {
		if samePath(path, d) {
			return true
		}
	}
	return false
}

// idMatch reports whether fi is a credential file.
func (g *guard) idMatch(fi fs.FileInfo) bool {
	for _, id := range g.ids {
		if os.SameFile(id, fi) {
			return true
		}
	}
	return false
}

func samePath(a, b string) bool {
	if foldCase {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// absRaw makes name absolute without cleaning it, so ".." keeps its
// physical meaning for resolveExisting.
func absRaw(name string) (string, error) {
	if filepath.IsAbs(name) {
		return name, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return wd + string(filepath.Separator) + name, nil
}

// resolveExisting resolves symlinks the way the kernel does, component by
// component, so "link/.." is the parent of the link's target. When the path
// does not exist, the longest existing prefix is resolved and the rest is
// appended.
func resolveExisting(p string) (string, error) {
	r, err := filepath.EvalSymlinks(p)
	if err == nil {
		return r, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	sep := string(filepath.Separator)
	end := len(p)
	for {
		i := strings.LastIndex(p[:end], sep)
		if i <= 0 {
			return filepath.Clean(p), nil
		}
		r, err := filepath.EvalSymlinks(p[:i])
		if err == nil {
			// The rest does not exist, so a ".." in it would fail in the
			// kernel anyway; cleaning it can only refuse more, not less.
			return filepath.Join(r, p[i+1:]), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		end = i
	}
}

// target is a checked path, ready for one operation.
type target struct {
	g    *guard
	real string // fully resolved path; the operation runs here
	link string // parent resolved, last element kept, for Lstat and Remove
}

func resolve(op, name string) (*target, error) {
	g, err := loadGuard()
	if err != nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	raw, err := absRaw(name)
	if err != nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	if g.pathMatch(filepath.Clean(raw)) {
		return nil, &AuthPathError{Op: op, Path: name}
	}
	real, err := resolveExisting(raw)
	if err != nil {
		return nil, &fs.PathError{Op: op, Path: name, Err: err}
	}
	if g.pathMatch(real) {
		return nil, &AuthPathError{Op: op, Path: name}
	}
	t := &target{g: g, real: real, link: real}
	if dir, base := filepath.Split(raw); base != "" && base != "." && base != ".." {
		parent, err := resolveExisting(dir)
		if err != nil {
			return nil, &fs.PathError{Op: op, Path: name, Err: err}
		}
		t.link = filepath.Join(parent, base)
		if g.pathMatch(t.link) {
			return nil, &AuthPathError{Op: op, Path: name}
		}
	}
	return t, nil
}

// renamePath reports errors under the caller's path, not the resolved one.
func renamePath(err error, name string) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		pe.Path = name
	}
	return err
}

// Check returns an error when name is, or resolves to, a vendor credential
// file, judged by path and, when it exists, by identity.
func Check(name string) error {
	t, err := resolve("Check", name)
	if err != nil {
		return err
	}
	for _, p := range []string{t.real, t.link} {
		if fi, err := os.Lstat(p); err == nil && t.g.idMatch(fi) {
			return &AuthPathError{Op: "Check", Path: name}
		}
	}
	if fi, err := os.Stat(t.real); err == nil && t.g.idMatch(fi) {
		return &AuthPathError{Op: "Check", Path: name}
	}
	return nil
}

// Open opens name for reading.
func Open(name string) (*os.File, error) { return openFile("Open", name, os.O_RDONLY, 0) }

// OpenFile is os.OpenFile behind both checks. The descriptor's identity is
// checked before it is returned, and O_TRUNC is applied only after that
// check passes.
func OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	return openFile("OpenFile", name, flag, perm)
}

func openFile(op, name string, flag int, perm fs.FileMode) (*os.File, error) {
	t, err := resolve(op, name)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(t.real); err == nil && t.g.idMatch(fi) {
		return nil, &AuthPathError{Op: op, Path: name}
	}
	if testHookBeforeOpen != nil {
		testHookBeforeOpen()
	}
	// The final element is already resolved, so a symlink there now means
	// it was swapped in after the check: refuse to follow it.
	f, err := os.OpenFile(t.real, (flag&^os.O_TRUNC)|oNoFollow, perm)
	if err != nil {
		return nil, renamePath(err, name)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, renamePath(err, name)
	}
	if t.g.idMatch(fi) {
		_ = f.Close()
		return nil, &AuthPathError{Op: op, Path: name}
	}
	if flag&os.O_TRUNC != 0 && fi.Mode().IsRegular() {
		if err := f.Truncate(0); err != nil {
			_ = f.Close()
			return nil, renamePath(err, name)
		}
	}
	return f, nil
}

// ReadFile reads the whole file through a checked descriptor.
func ReadFile(name string) ([]byte, error) {
	f, err := openFile("ReadFile", name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// ReadDir reads a directory through a checked descriptor and returns its
// entries sorted by name, like os.ReadDir.
func ReadDir(name string) ([]fs.DirEntry, error) {
	f, err := openFile("ReadDir", name, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(-1)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, renamePath(err, name)
}

// WriteFile writes data atomically: it writes a temp file in the target's
// directory, syncs it, renames it over the target, and syncs the directory.
// An existing file keeps its mode and perm is ignored; a new file gets
// perm. When name is a symlink, the file it points to is replaced and the
// link is kept.
func WriteFile(name string, data []byte, perm fs.FileMode) error {
	t, err := resolve("WriteFile", name)
	if err != nil {
		return err
	}
	fi, err := os.Lstat(t.real)
	switch {
	case err == nil:
		if t.g.idMatch(fi) {
			return &AuthPathError{Op: "WriteFile", Path: name}
		}
		if !fi.Mode().IsRegular() {
			return &fs.PathError{Op: "WriteFile", Path: name, Err: errors.New("not a regular file")}
		}
		perm = fi.Mode().Perm()
	case !errors.Is(err, fs.ErrNotExist):
		return renamePath(err, name)
	}
	dir := filepath.Dir(t.real)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(t.real)+".aggy-*")
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
	// Re-check right before the rename to narrow the window for a swap.
	if fi, err := os.Lstat(t.real); err == nil && t.g.idMatch(fi) {
		return &AuthPathError{Op: "WriteFile", Path: name}
	}
	if err := os.Rename(tmp.Name(), t.real); err != nil {
		return err
	}
	ok = true
	return syncDir(dir)
}

// syncDir makes a rename in dir durable. File systems that cannot sync a
// directory are tolerated.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

// Stat is os.Stat behind both checks.
func Stat(name string) (fs.FileInfo, error) {
	t, err := resolve("Stat", name)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(t.real)
	if err != nil {
		return nil, renamePath(err, name)
	}
	if t.g.idMatch(fi) {
		return nil, &AuthPathError{Op: "Stat", Path: name}
	}
	return fi, nil
}

// Lstat is os.Lstat behind both checks.
func Lstat(name string) (fs.FileInfo, error) {
	t, err := resolve("Lstat", name)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(t.link)
	if err != nil {
		return nil, renamePath(err, name)
	}
	if t.g.idMatch(fi) {
		return nil, &AuthPathError{Op: "Lstat", Path: name}
	}
	return fi, nil
}

// MkdirAll is os.MkdirAll behind both checks.
func MkdirAll(name string, perm fs.FileMode) error {
	t, err := resolve("MkdirAll", name)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(t.real); err == nil && t.g.idMatch(fi) {
		return &AuthPathError{Op: "MkdirAll", Path: name}
	}
	return renamePath(os.MkdirAll(t.real, perm), name)
}

// Remove removes the named file or empty directory, or the link itself
// when name is a symlink, behind both checks.
func Remove(name string) error {
	t, err := resolve("Remove", name)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(t.link); err == nil && t.g.idMatch(fi) {
		return &AuthPathError{Op: "Remove", Path: name}
	}
	return renamePath(os.Remove(t.link), name)
}

// Chmod is os.Chmod behind both checks.
func Chmod(name string, mode fs.FileMode) error {
	t, err := resolve("Chmod", name)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(t.real); err == nil && t.g.idMatch(fi) {
		return &AuthPathError{Op: "Chmod", Path: name}
	}
	return renamePath(os.Chmod(t.real, mode), name)
}
