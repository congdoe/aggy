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
// Every operation then acts relative to a descriptor for the checked
// parent directory, whose identity is compared with the one checked, so a
// directory swapped for a symlink after the check cannot redirect it.
// Opens and Chmod never follow a symlink at the final element, and Chmod
// changes the mode through the checked descriptor.
//
// A regular file with more than one hard link is refused for opening and
// Chmod (ErrHardLink): its other name could be a credential file that the
// identity check does not know, such as one created after the deny list
// was loaded. aggy never hard-links its own files.
//
// A credential file that does not exist has nothing to protect and matches
// only by path.
//
// safefs also remembers every device and inode it has seen for a
// credential file, in $AGGY_HOME/credential-ids (see HistoryFile), and
// refuses a file with any of them, so a hard link made before a token
// rotation is still refused after it. A corrupt history fails every call
// with ErrHistory; a missing one is rebuilt from the current files.
//
// safefs fails closed. When no absolute home directory can be found, or
// CLAUDE_CONFIG_DIR, CODEX_HOME, or GROK_HOME is set to a relative path,
// every call returns an error matching ErrLocate.
package safefs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
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

// passwdHome looks up the user database once per process.
var passwdHome = sync.OnceValues(func() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
})

// lookupUserHome returns the home directory from the user database, which
// the guard covers as well as $HOME. In a test binary it is not consulted
// (it reports an error), so tests never stat the real credential files;
// safefs's own tests replace it to exercise both homes.
var lookupUserHome = func() (string, error) {
	if testing.Testing() {
		return "", errors.New("the user database is not consulted in tests")
	}
	return passwdHome()
}

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

// homeDirs returns every home directory a vendor CLI might use: $HOME,
// made absolute against the working directory when it is relative, and the
// home in the user database when it differs. A child that changes HOME
// therefore cannot hide the real home's credential files. With neither,
// it fails closed.
func homeDirs() ([]string, error) {
	var hs []string
	if h := os.Getenv("HOME"); h != "" {
		if !filepath.IsAbs(h) {
			if wd, err := os.Getwd(); err == nil {
				h = filepath.Join(wd, h)
			} else {
				h = ""
			}
		}
		if h != "" {
			hs = append(hs, filepath.Clean(h))
		}
	}
	if h, err := lookupUserHome(); err == nil && h != "" && filepath.IsAbs(h) {
		h = filepath.Clean(h)
		if len(hs) == 0 || hs[0] != h {
			hs = append(hs, h)
		}
	}
	if len(hs) == 0 {
		return nil, fmt.Errorf("%w: no absolute home directory", ErrLocate)
	}
	return hs, nil
}

// guard is the deny list for one call. It is rebuilt each call because the
// environment can change.
type guard struct {
	paths []string      // credential paths, normalized by normPath
	ids   []fs.FileInfo // identities of the credential files that exist
	hist  []id          // every identity seen for a credential file (history.go)
}

func loadGuard() (*guard, error) {
	homes, err := homeDirs()
	if err != nil {
		return nil, err
	}
	var creds []string
	for _, c := range credDirs {
		var dirs []string
		for _, h := range homes {
			dirs = append(dirs, filepath.Join(h, c.dir))
		}
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
		g.paths = append(g.paths, normPath(p))
		if d, err := resolveExisting(filepath.Dir(p)); err == nil {
			g.paths = append(g.paths, normPath(filepath.Join(d, filepath.Base(p))))
		}
		// Metadata only: these calls never read the file's contents.
		if fi, err := os.Lstat(p); err == nil {
			g.ids = append(g.ids, fi)
		}
		if fi, err := os.Stat(p); err == nil {
			g.ids = append(g.ids, fi)
		}
	}
	var seen []id
	for _, fi := range g.ids {
		if i, ok := fileID(fi); ok && fi.Mode().IsRegular() {
			seen = append(seen, i)
		}
	}
	if g.hist, err = g.loadHistory(seen); err != nil {
		return nil, err
	}
	return g, nil
}

// pathMatch reports whether path names a credential file, or a path
// under one (which can only exist if the credential is a directory, or
// would be created in its place). Paths compare in Unicode NFC, and
// case-insensitively where the file system ignores case, so a credential
// that does not exist yet, and so has no identity to check, is still
// matched when spelled another way.
func (g *guard) pathMatch(path string) bool {
	n := normPath(path)
	for _, d := range g.paths {
		if n == d || strings.HasPrefix(n, d+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// normPath is the lexical form paths are compared in: NFC, and, where the
// file system ignores case, full Unicode case folding followed by NFC
// again. Full folding matches what APFS treats as one name: "ſ" (long s)
// folds to "s", "ß" to "ss", and the Kelvin sign to "k", where
// strings.ToLower would leave all three alone.
func normPath(p string) string {
	p = norm.NFC.String(p)
	if foldCase {
		p = norm.NFC.String(cases.Fold().String(p))
	}
	return p
}

// CanonicalPath returns the one spelling of name that every other
// spelling of the same file shares: made absolute, resolved through
// symlinks the way the kernel resolves them (the deepest existing
// ancestor, then the rest appended), and normalized the way safefs
// compares paths (NFC, and case-folded where the file system ignores
// case). It is for keys such as lock names, never for opening the file.
// It refuses a credential path like every other call.
func CanonicalPath(name string) (string, error) {
	t, err := resolve("CanonicalPath", name)
	if err != nil {
		return "", err
	}
	return normPath(t.real), nil
}

// under reports whether the normalized path d is p or lies below it.
func under(d, p string) bool {
	if p == string(filepath.Separator) {
		return true
	}
	return d == p || strings.HasPrefix(d, p+string(filepath.Separator))
}

// idMatch reports whether fi is a credential file, or has the identity a
// credential file had before a rotation (history.go).
func (g *guard) idMatch(fi fs.FileInfo) bool {
	for _, id := range g.ids {
		if os.SameFile(id, fi) {
			return true
		}
	}
	if i, ok := fileID(fi); ok && slices.Contains(g.hist, i) {
		return true
	}
	return false
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
	// The identities of real's and link's parent directories, taken when
	// the path was checked; inParent acts only in those directories.
	realDir, linkDir parentID
}

// parentID is a directory's identity at check time, or the error from
// looking it up.
type parentID struct {
	fi  fs.FileInfo
	err error
}

func lstatDir(p string) parentID {
	fi, err := os.Lstat(filepath.Dir(p))
	return parentID{fi, err}
}

// testHookAfterResolve, when set by a test, runs right after resolve has
// checked a path and recorded its parents' identities.
var testHookAfterResolve func(op string)

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
	t.realDir = lstatDir(t.real)
	t.linkDir = t.realDir
	if filepath.Dir(t.link) != filepath.Dir(t.real) {
		t.linkDir = lstatDir(t.link)
	}
	if testHookAfterResolve != nil {
		testHookAfterResolve(op)
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

// ErrHardLink matches the refusal of a regular file that has more than one
// link. A credential file can gain a second name before safefs learns its
// identity, for example when it is created after the deny list was loaded;
// refusing every multiply-linked file closes that. aggy never hard-links
// its own files.
//
// A hard link made to a credential before a token rotation that replaces
// the credential by rename keeps the old inode, which then has one link
// left and holds the old token. That file is refused by identity, with
// ErrAuthPath, through the identity history (history.go), as long as
// safefs saw the old inode before the rotation.
var ErrHardLink = errors.New("refusing a file with more than one hard link")

// ErrChanged means the file system changed between the checks and the
// operation, for example a directory replaced by a symlink.
var ErrChanged = errors.New("path changed during the call")

// ErrDirSync matches, through errors.Is, a *DirSyncError.
var ErrDirSync = errors.New("file written but its directory could not be synced")

// DirSyncError reports a WriteFile whose rename succeeded but whose
// directory sync failed. The new content is in place and visible; only its
// durability across a power loss is in doubt. Callers that record the write
// must still record it.
type DirSyncError struct {
	Path string // the path as the caller gave it
	Err  error  // the sync error
}

func (e *DirSyncError) Error() string {
	return fmt.Sprintf("safefs: WriteFile %s: %v: %v", e.Path, ErrDirSync, e.Err)
}

// Unwrap returns the sync error.
func (e *DirSyncError) Unwrap() error { return e.Err }

// Is reports whether target is ErrDirSync.
func (e *DirSyncError) Is(target error) bool { return target == ErrDirSync }

// testHookBeforeAct, when set by a test, runs after an operation's checks
// and before it acts, so a test can swap the file system in exactly that
// window. op is the safefs function name.
var testHookBeforeAct func(op string)

// testSyncDir, when set by a test, replaces the directory sync in WriteFile.
var testSyncDir func() error

// testSyncDirPath, set through SetTestDirSync, replaces the directory sync
// in WriteFile for tests in other packages; it gets the directory's path.
var testSyncDirPath func(dir string) error

// SetTestDirSync makes WriteFile's directory sync call fn with the
// directory's path instead, so tests in other packages can make a sync
// fail. It panics outside a test binary. The returned func restores it.
func SetTestDirSync(fn func(dir string) error) (restore func()) {
	if !testing.Testing() {
		panic("safefs: SetTestDirSync outside a test")
	}
	old := testSyncDirPath
	testSyncDirPath = fn
	return func() { testSyncDirPath = old }
}

func beforeAct(op string) {
	if testHookBeforeAct != nil {
		testHookBeforeAct(op)
	}
}

// inParent runs fn on the last element of p, through a descriptor for p's
// directory. want is the directory's identity from resolve, taken when the
// path was checked, and is compared after the open, so a directory replaced
// in between (by a symlink or another directory) is refused, and the
// operation then works relative to that descriptor, so a later swap of the
// directory path cannot redirect it.
func inParent(op, name, p string, want parentID, fn func(dir *os.Root, base string) error) error {
	dir, base := filepath.Dir(p), filepath.Base(p)
	if dir == p {
		base = "."
	}
	if want.err != nil {
		return renamePath(want.err, name)
	}
	if !want.fi.IsDir() {
		return &fs.PathError{Op: op, Path: name, Err: ErrChanged}
	}
	beforeAct(op)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return renamePath(err, name)
	}
	defer func() { _ = root.Close() }()
	got, err := root.Stat(".")
	if err != nil {
		return renamePath(err, name)
	}
	if !os.SameFile(want.fi, got) {
		return &fs.PathError{Op: op, Path: name, Err: ErrChanged}
	}
	return fn(root, base)
}

// isMultiLinked reports whether fi is a regular file with another name.
func isMultiLinked(fi fs.FileInfo) bool {
	return fi.Mode().IsRegular() && linkCount(fi) > 1
}

// Open opens name for reading.
func Open(name string) (*os.File, error) { return openFile("Open", name, os.O_RDONLY, 0) }

// OpenFile is os.OpenFile behind both checks. The descriptor's identity is
// checked before it is returned, and O_TRUNC is applied only after that
// check passes. A regular file with more than one link is refused with
// ErrHardLink.
func OpenFile(name string, flag int, perm fs.FileMode) (*os.File, error) {
	return openFile("OpenFile", name, flag, perm)
}

func openFile(op, name string, flag int, perm fs.FileMode) (*os.File, error) {
	return openFileIn(op, name, flag, perm, nil)
}

// openFileIn is openFile that, when wantDir is not nil, also requires the
// checked parent directory to be wantDir.
func openFileIn(op, name string, flag int, perm fs.FileMode, wantDir fs.FileInfo) (*os.File, error) {
	t, err := resolve(op, name)
	if err != nil {
		return nil, err
	}
	if wantDir != nil && (t.realDir.err != nil || !os.SameFile(wantDir, t.realDir.fi)) {
		return nil, &fs.PathError{Op: op, Path: name, Err: ErrChanged}
	}
	if fi, err := os.Stat(t.real); err == nil && t.g.idMatch(fi) {
		return nil, &AuthPathError{Op: op, Path: name}
	}
	var f *os.File
	err = inParent(op, name, t.real, t.realDir, func(dir *os.Root, base string) error {
		// The final element is already resolved, so a symlink there now
		// means it was swapped in after the check: refuse to follow it.
		var err error
		for range 5 {
			f, err = dir.OpenFile(base, (flag&^os.O_TRUNC)|oNoFollow, perm)
			// os.Root.OpenFile with O_CREATE can fail with ENOENT when
			// another process creates the same file at that moment
			// (seen with go1.27 on darwin); the retry then opens it.
			if err == nil || flag&os.O_CREATE == 0 || !errors.Is(err, fs.ErrNotExist) {
				break
			}
		}
		return err
	})
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
	if isMultiLinked(fi) {
		_ = f.Close()
		return nil, &fs.PathError{Op: op, Path: name, Err: ErrHardLink}
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

// ErrTooLarge means a file is larger than the caller's limit.
var ErrTooLarge = errors.New("file is larger than the limit")

// ErrNotRegular means a file is a symlink, directory, device, or other
// non-regular file where a regular file is required.
var ErrNotRegular = errors.New("not a regular file")

// ReadFileLimit reads a regular file of at most max bytes through a
// checked descriptor. Unlike ReadFile, it refuses a name that is itself a
// symlink, and any file that is not regular (ErrNotRegular), and a file
// over max bytes (ErrTooLarge). It is for small files whose content aggy
// trusts to parse, such as config.
func ReadFileLimit(name string, max int64) ([]byte, error) {
	return readFileLimit("ReadFileLimit", name, max, nil)
}

// ErrLinkedDir means a file's own directory is a symlink where aggy
// requires a real directory.
var ErrLinkedDir = errors.New("the file's directory is a symlink")

// ReadConfig is ReadFileLimit for config files. It also refuses, with
// ErrLinkedDir, a file whose own directory is a symlink, such as a cloned
// repo's .aggy pointing somewhere else on the disk. The directory checked
// is the one the file is read from: a directory swapped for a symlink
// after the check is refused with ErrChanged. Directories further up may
// be symlinks.
func ReadConfig(name string, max int64) ([]byte, error) {
	// The file is checked first, so a credential path is refused as such
	// even when its directory does not exist.
	if err := Check(name); err != nil {
		return nil, err
	}
	dfi, err := Lstat(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	if dfi.Mode()&fs.ModeSymlink != 0 || !dfi.IsDir() {
		return nil, &fs.PathError{Op: "ReadConfig", Path: name, Err: ErrLinkedDir}
	}
	return readFileLimit("ReadConfig", name, max, dfi)
}

func readFileLimit(op, name string, max int64, wantDir fs.FileInfo) ([]byte, error) {
	fi, err := Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, &fs.PathError{Op: op, Path: name, Err: ErrNotRegular}
	}
	f, err := openFileIn(op, name, os.O_RDONLY|oNonBlock, 0, wantDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	fi, err = f.Stat()
	if err != nil {
		return nil, renamePath(err, name)
	}
	if !fi.Mode().IsRegular() {
		return nil, &fs.PathError{Op: op, Path: name, Err: ErrNotRegular}
	}
	if fi.Size() > max {
		return nil, &fs.PathError{Op: op, Path: name, Err: ErrTooLarge}
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, renamePath(err, name)
	}
	if int64(len(b)) > max {
		return nil, &fs.PathError{Op: op, Path: name, Err: ErrTooLarge}
	}
	return b, nil
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
// Every step works relative to one checked descriptor for that directory.
// An existing file keeps its mode and perm is ignored; a new file gets
// perm. When name is a symlink, the file it points to is replaced and the
// link is kept.
//
// When the rename succeeds but the directory sync fails, the new content is
// in place, and WriteFile returns a *DirSyncError (matching ErrDirSync).
// Callers treat that as a write that happened.
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
	var syncErr error
	err = inParent("WriteFile", name, t.real, t.realDir, func(dir *os.Root, base string) error {
		// Re-check through the descriptor: the name may have been swapped.
		if fi, err := dir.Lstat(base); err == nil {
			if t.g.idMatch(fi) {
				return &AuthPathError{Op: "WriteFile", Path: name}
			}
			if !fi.Mode().IsRegular() {
				return &fs.PathError{Op: "WriteFile", Path: name, Err: ErrChanged}
			}
		}
		tmpName, tmp, err := createTemp(dir, base)
		if err != nil {
			return err
		}
		ok := false
		defer func() {
			if !ok {
				_ = tmp.Close()
				_ = dir.Remove(tmpName)
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
		if fi, err := dir.Lstat(base); err == nil && t.g.idMatch(fi) {
			return &AuthPathError{Op: "WriteFile", Path: name}
		}
		if err := dir.Rename(tmpName, base); err != nil {
			return err
		}
		ok = true
		syncErr = syncDir(dir)
		return nil
	})
	if err != nil {
		return renamePath(err, name)
	}
	if syncErr != nil {
		return &DirSyncError{Path: name, Err: syncErr}
	}
	return nil
}

// createTemp creates a new, empty, exclusive temp file next to base.
func createTemp(dir *os.Root, base string) (string, *os.File, error) {
	for range 100 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", nil, err
		}
		name := "." + base + ".aggy-" + hex.EncodeToString(b[:])
		f, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return name, f, err
	}
	return "", nil, errors.New("safefs: cannot create a temp file")
}

// syncDir makes a rename in dir durable. File systems that cannot sync a
// directory are tolerated.
func syncDir(dir *os.Root) error {
	if testSyncDir != nil {
		return testSyncDir()
	}
	if testSyncDirPath != nil {
		return testSyncDirPath(dir.Name())
	}
	d, err := dir.Open(".")
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

// Stat is os.Stat behind both checks. The final element is not followed
// again after the check: if it has become a symlink since, Stat fails with
// ErrChanged rather than describe what the link now points to.
func Stat(name string) (fs.FileInfo, error) {
	t, err := resolve("Stat", name)
	if err != nil {
		return nil, err
	}
	var fi fs.FileInfo
	err = inParent("Stat", name, t.real, t.realDir, func(dir *os.Root, base string) error {
		var err error
		if fi, err = dir.Lstat(base); err != nil {
			return err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			// The checked path was fully resolved. A dangling link there was
			// already one at check time and reads as missing, as os.Stat
			// would report; any other link is a swap. Only the error is
			// used, never what the link points to.
			if _, err := os.Stat(t.real); errors.Is(err, fs.ErrNotExist) {
				return err
			}
			return ErrChanged
		}
		return nil
	})
	if err != nil {
		return nil, wrapPath("Stat", name, err)
	}
	if t.g.idMatch(fi) {
		return nil, &AuthPathError{Op: "Stat", Path: name}
	}
	return fi, nil
}

// wrapPath gives a bare sentinel a path, and renames a *PathError's path.
func wrapPath(op, name string, err error) error {
	var pe *fs.PathError
	var ape *AuthPathError
	if errors.As(err, &pe) || errors.As(err, &ape) {
		return renamePath(err, name)
	}
	return &fs.PathError{Op: op, Path: name, Err: err}
}

// Lstat is os.Lstat behind both checks.
func Lstat(name string) (fs.FileInfo, error) {
	t, err := resolve("Lstat", name)
	if err != nil {
		return nil, err
	}
	var fi fs.FileInfo
	err = inParent("Lstat", name, t.link, t.linkDir, func(dir *os.Root, base string) error {
		var err error
		fi, err = dir.Lstat(base)
		return err
	})
	if err != nil {
		return nil, renamePath(err, name)
	}
	if t.g.idMatch(fi) {
		return nil, &AuthPathError{Op: "Lstat", Path: name}
	}
	return fi, nil
}

// MkdirAll is os.MkdirAll behind both checks. It creates the missing
// directories relative to a checked descriptor for the deepest one that
// exists, so a directory swapped for a symlink cannot redirect it.
func MkdirAll(name string, perm fs.FileMode) error {
	t, err := resolve("MkdirAll", name)
	if err != nil {
		return err
	}
	// Find the deepest existing directory. t.real is resolved, so it has no
	// symlinks.
	anc := t.real
	var fi fs.FileInfo
	for {
		fi, err = os.Lstat(anc)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return renamePath(err, name)
		}
		up := filepath.Dir(anc)
		if up == anc {
			return renamePath(err, name)
		}
		anc = up
	}
	if t.g.idMatch(fi) {
		return &AuthPathError{Op: "MkdirAll", Path: name}
	}
	if anc == t.real {
		if !fi.IsDir() {
			return &fs.PathError{Op: "MkdirAll", Path: name, Err: syscall.ENOTDIR}
		}
		return nil
	}
	if !fi.IsDir() {
		return &fs.PathError{Op: "MkdirAll", Path: name, Err: syscall.ENOTDIR}
	}
	rel, err := filepath.Rel(anc, t.real)
	if err != nil {
		return err
	}
	beforeAct("MkdirAll")
	root, err := os.OpenRoot(anc)
	if err != nil {
		return renamePath(err, name)
	}
	defer func() { _ = root.Close() }()
	got, err := root.Stat(".")
	if err != nil {
		return renamePath(err, name)
	}
	if !os.SameFile(fi, got) {
		return &fs.PathError{Op: "MkdirAll", Path: name, Err: ErrChanged}
	}
	return renamePath(root.MkdirAll(rel, perm), name)
}

// Remove removes the named file or empty directory, or the link itself
// when name is a symlink, behind both checks. The removal works relative to
// a checked descriptor for the parent directory.
func Remove(name string) error {
	t, err := resolve("Remove", name)
	if err != nil {
		return err
	}
	return renamePath(inParent("Remove", name, t.link, t.linkDir, func(dir *os.Root, base string) error {
		if fi, err := dir.Lstat(base); err == nil && t.g.idMatch(fi) {
			return &AuthPathError{Op: "Remove", Path: name}
		}
		return dir.Remove(base)
	}), name)
}

// Chmod is os.Chmod behind both checks. It opens the file without following
// a final symlink, checks the descriptor's identity, and changes the mode
// through the descriptor, so the file whose mode changes is the file that
// was checked. A regular file with more than one link is refused with
// ErrHardLink. The file must be readable or writable by aggy.
func Chmod(name string, mode fs.FileMode) error {
	t, err := resolve("Chmod", name)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(t.real); err == nil && t.g.idMatch(fi) {
		return &AuthPathError{Op: "Chmod", Path: name}
	}
	var f *os.File
	err = inParent("Chmod", name, t.real, t.realDir, func(dir *os.Root, base string) error {
		var err error
		f, err = dir.OpenFile(base, os.O_RDONLY|oNoFollow|oNonBlock, 0)
		if errors.Is(err, fs.ErrPermission) {
			f, err = dir.OpenFile(base, os.O_WRONLY|oNoFollow|oNonBlock, 0)
		}
		return err
	})
	if err != nil {
		return renamePath(err, name)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return renamePath(err, name)
	}
	if t.g.idMatch(fi) {
		return &AuthPathError{Op: "Chmod", Path: name}
	}
	if isMultiLinked(fi) {
		return &fs.PathError{Op: "Chmod", Path: name, Err: ErrHardLink}
	}
	return renamePath(f.Chmod(mode), name)
}

// MkdirTemp creates a new directory with mode 0700 in dir (the system temp
// dir when dir is empty), like os.MkdirTemp, behind both checks.
func MkdirTemp(dir, pattern string) (string, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	t, err := resolve("MkdirTemp", dir)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(t.real); err == nil && t.g.idMatch(fi) {
		return "", &AuthPathError{Op: "MkdirTemp", Path: dir}
	}
	return os.MkdirTemp(t.real, pattern)
}

// RemoveAll removes name and everything under it, behind both checks, and
// working relative to a checked descriptor for name's parent: os.Root
// never follows a symlink out of that directory, so a link inside the tree
// is removed, not its target. Removing a hard link's name never removes
// the file's data from its other names.
//
// The checks cover the whole tree, not only name. The call is refused,
// and nothing is removed, when a credential path lies under name
// (compared lexically, after normalization, whether or not the credential
// exists), or when any entry in the tree, walked before anything is
// removed, has the identity of a credential file. So RemoveAll(~/.codex)
// is refused.
func RemoveAll(name string) error {
	t, err := resolve("RemoveAll", name)
	if err != nil {
		return err
	}
	for _, p := range []string{t.real, t.link} {
		np := normPath(p)
		for _, d := range t.g.paths {
			if under(d, np) {
				return &AuthPathError{Op: "RemoveAll", Path: name}
			}
		}
	}
	return renamePath(inParent("RemoveAll", name, t.link, t.linkDir, func(dir *os.Root, base string) error {
		if base == "." || base == ".." {
			return &fs.PathError{Op: "RemoveAll", Path: name, Err: errors.New("refusing to remove a dot entry")}
		}
		if err := t.g.walkTree(dir, base, 0); err != nil {
			if errors.Is(err, ErrAuthPath) {
				return &AuthPathError{Op: "RemoveAll", Path: name}
			}
			return err
		}
		return dir.RemoveAll(base)
	}), name)
}

// maxWalkDepth bounds the tree RemoveAll checks. A deeper tree is refused
// rather than removed unchecked.
const maxWalkDepth = 256

// walkTree checks base in dir and, for a directory, everything under it,
// by identity, without following symlinks. A missing entry is fine.
func (g *guard) walkTree(dir *os.Root, base string, depth int) error {
	if depth > maxWalkDepth {
		return fmt.Errorf("safefs: RemoveAll: tree is deeper than %d levels", maxWalkDepth)
	}
	fi, err := dir.Lstat(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if g.idMatch(fi) {
		return ErrAuthPath
	}
	if !fi.IsDir() {
		return nil
	}
	sub, err := dir.OpenRoot(base)
	if err != nil {
		return err
	}
	defer func() { _ = sub.Close() }()
	got, err := sub.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(fi, got) {
		return ErrChanged
	}
	d, err := sub.Open(".")
	if err != nil {
		return err
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := g.walkTree(sub, e.Name(), depth+1); err != nil {
			return err
		}
	}
	return nil
}
