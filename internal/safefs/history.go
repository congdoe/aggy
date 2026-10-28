package safefs

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/congdoe/aggy/internal/home"
)

// The identity history closes the hard-link-before-rotation gap. A vendor
// that rotates its token writes a new file and renames it over the old
// one, so the credential gets a new inode. A hard link made to the old
// file before that keeps the old inode, now with one link and the old
// token, and the current identities no longer name it. safefs therefore
// remembers every (device, inode) it has seen for a credential file, in
// a small file it manages itself, and refuses any file with one of them.
// While the extra link exists the old inode cannot be reused, so the
// check is exact; after the link is gone, a new file may get the same
// inode and be refused too, which fails closed.
//
// The history covers what safefs has seen. A hard link made to a token
// that was rotated away before any aggy call saw it is not covered.

// HistoryFile is the identity history inside the state directory
// ($AGGY_HOME or ~/.aggy). It holds device and inode numbers only.
const HistoryFile = "credential-ids"

// historyHeader is the first line of the history file.
const historyHeader = "aggy credential identities v1"

// maxHistory caps the history. Past it, the oldest entries are dropped:
// a token rotated that many times ago is the least likely to still have
// a stray link.
const maxHistory = 1024

// maxHistoryBytes bounds the history file read; a larger file is corrupt.
const maxHistoryBytes = 64 << 10

// ErrHistory matches a history file safefs cannot trust. Every call then
// fails closed, because the identities it held are unknown. Deleting the
// file rebuilds it from the credential files that exist now.
var ErrHistory = errors.New("credential identity history is unreadable")

// id is a file's device and inode.
type id struct{ dev, ino uint64 }

// historyPath returns the history file's path.
func historyPath() (string, error) {
	dir, err := home.Dir()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrLocate, err)
	}
	return filepath.Join(dir, HistoryFile), nil
}

// loadHistory returns the remembered identities merged with seen, the
// identities of the credential files that exist now, and saves the merge
// when seen adds to it.
//
// A missing history file is rebuilt from seen; it is created only when
// there is something to record. A corrupt one (a bad header or line, a
// symlink, or a file over maxHistoryBytes) is an error matching
// ErrHistory: rebuilding it would silently forget the old identities.
// When the merged history cannot be saved (a read-only state directory),
// the call goes on with the merge in memory, which is never less than the
// current identities.
//
// Two processes adding different identities at once can lose one of them
// from the file. An identity that is still current is added again on the
// next call, so only a rotation in that same moment can be missed.
func (g *guard) loadHistory(seen []id) ([]id, error) {
	path, err := historyPath()
	if err != nil {
		return nil, err
	}
	if g.pathMatch(path) {
		return nil, fmt.Errorf("%w: %s is a credential path", ErrHistory, path)
	}
	hist, err := readHistory(path)
	if err != nil {
		return nil, err
	}
	merged := slices.Clone(hist)
	for _, s := range seen {
		if !slices.Contains(merged, s) {
			merged = append(merged, s)
		}
	}
	if len(merged) == len(hist) {
		return merged, nil
	}
	if len(merged) > maxHistory {
		merged = merged[len(merged)-maxHistory:]
	}
	_ = writeHistory(path, merged)
	return merged, nil
}

// readHistory reads the history file. A missing file is an empty history.
func readHistory(path string) ([]id, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|oNoFollow|oNonBlock, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, historyErr(path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return nil, historyErr(path, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, historyErr(path, ErrNotRegular)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxHistoryBytes+1))
	if err != nil {
		return nil, historyErr(path, err)
	}
	if len(b) > maxHistoryBytes {
		return nil, historyErr(path, ErrTooLarge)
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	if !sc.Scan() || sc.Text() != historyHeader {
		return nil, historyErr(path, errors.New("bad header"))
	}
	var out []id
	for n := 2; sc.Scan(); n++ {
		dev, ino, ok := strings.Cut(sc.Text(), " ")
		d, derr := strconv.ParseUint(dev, 10, 64)
		i, ierr := strconv.ParseUint(ino, 10, 64)
		if !ok || derr != nil || ierr != nil {
			return nil, historyErr(path, fmt.Errorf("bad line %d", n))
		}
		out = append(out, id{dev: d, ino: i})
	}
	return out, nil
}

func historyErr(path string, err error) error {
	return fmt.Errorf("%w: %s: %w (delete it to rebuild it from the credential files that exist now)", ErrHistory, path, err)
}

// writeHistory replaces the history file atomically, mode 0600, creating
// the state directory 0700 when it is missing.
func writeHistory(path string, ids []id) error {
	var b strings.Builder
	b.WriteString(historyHeader + "\n")
	for _, i := range ids {
		fmt.Fprintf(&b, "%d %d\n", i.dev, i.ino)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	tmp := "." + HistoryFile + ".aggy-" + hex.EncodeToString(rnd[:])
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|oNoFollow, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(b.String())
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = root.Rename(tmp, HistoryFile)
	}
	if err != nil {
		_ = root.Remove(tmp)
		return err
	}
	return syncDir(root)
}
