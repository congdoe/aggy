//go:build unix

package safefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// rotate replaces path the way a vendor rotates its token: a new file
// renamed over the old one, so the credential gets a new inode.
func rotate(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp"
	must(t, os.WriteFile(tmp, []byte(content), 0o600))
	must(t, os.Rename(tmp, path))
}

// TestHardLinkBeforeRotation (T6724): a hard link made to a credential
// before a token rotation keeps the old inode, with one link and the old
// token. On 2e1e234 ReadFile(link) returned the old token.
func TestHardLinkBeforeRotation(t *testing.T) {
	home := fakeHome(t)
	auth := filepath.Join(home, ".codex", "auth.json")
	writeFake(t, auth)
	// Any call loads the guard, which records the credential's identity.
	if err := Check(filepath.Join(t.TempDir(), "x")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "old-token.png")
	if err := os.Link(auth, link); err != nil {
		t.Skipf("hard links not supported here: %v", err)
	}
	rotate(t, auth, "new token")
	if fi, err := os.Lstat(link); err != nil || linkCount(fi) != 1 {
		t.Fatalf("link count after rotation = %v, %v; want 1", fi, err)
	}
	b, err := ReadFile(link)
	if !errors.Is(err, ErrAuthPath) {
		t.Errorf("ReadFile(link after rotation) = %q, %v, want ErrAuthPath", b, err)
	}
	assertRefused(t, link)
	// A copy holding the same bytes is a different file and is allowed:
	// identity, not content, is what is refused.
	other := filepath.Join(t.TempDir(), "other")
	must(t, os.WriteFile(other, []byte("unrelated"), 0o600))
	if _, err := ReadFile(other); err != nil {
		t.Errorf("ReadFile(unrelated) = %v", err)
	}
}

// TestHistoryFile: the history is a 0600 file in the state directory that
// survives the process, is rebuilt when missing, and fails closed when
// corrupt.
func TestHistoryFile(t *testing.T) {
	home := fakeHome(t)
	path, err := historyPath()
	if err != nil {
		t.Fatal(err)
	}
	// No credential files: nothing to record, and no file is created.
	must(t, Check(filepath.Join(t.TempDir(), "x")))
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("history created with nothing to record: %v", err)
	}
	auth := filepath.Join(home, ".grok", "auth.json")
	writeFake(t, auth)
	must(t, Check(filepath.Join(t.TempDir(), "x")))
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("history mode = %v, want 0600", fi.Mode().Perm())
	}
	b, _ := os.ReadFile(path)
	if lines := strings.Split(strings.TrimSpace(string(b)), "\n"); lines[0] != historyHeader || len(lines) != 2 {
		t.Errorf("history = %q, want the header and one identity", b)
	}
	// A rotation adds the new identity and keeps the old one.
	rotate(t, auth, "rotated")
	must(t, Check(filepath.Join(t.TempDir(), "x")))
	b, _ = os.ReadFile(path)
	if n := strings.Count(string(b), "\n"); n != 3 {
		t.Errorf("history after rotation = %q, want two identities", b)
	}
	// Missing: rebuilt from the current files.
	must(t, os.Remove(path))
	must(t, Check(filepath.Join(t.TempDir(), "x")))
	if b, _ := os.ReadFile(path); strings.Count(string(b), "\n") != 2 {
		t.Errorf("rebuilt history = %q, want one identity", b)
	}
	// Corrupt: every call fails closed and says what to do.
	for _, bad := range []string{"garbage\n", historyHeader + "\n1 x\n", historyHeader + "\n" + strings.Repeat("1 2\n", maxHistoryBytes)} {
		must(t, os.WriteFile(path, []byte(bad), 0o600))
		err := Check(filepath.Join(t.TempDir(), "x"))
		if !errors.Is(err, ErrHistory) || !strings.Contains(err.Error(), "delete it") {
			t.Errorf("corrupt history %.20q: Check = %v, want ErrHistory", bad, err)
		}
		if _, err := ReadFile(filepath.Join(t.TempDir(), "x")); !errors.Is(err, ErrHistory) {
			t.Errorf("corrupt history: ReadFile = %v, want ErrHistory", err)
		}
	}
	// A symlink in its place is refused, not followed.
	must(t, os.Remove(path))
	target := filepath.Join(t.TempDir(), "elsewhere")
	must(t, os.WriteFile(target, []byte(historyHeader+"\n"), 0o600))
	must(t, os.Symlink(target, path))
	if err := Check(filepath.Join(t.TempDir(), "x")); !errors.Is(err, ErrHistory) {
		t.Errorf("symlinked history: Check = %v, want ErrHistory", err)
	}
}

// TestHistoryCap: past maxHistory the oldest identities are dropped.
func TestHistoryCap(t *testing.T) {
	fakeHome(t)
	path, err := historyPath()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(historyHeader + "\n")
	for i := range maxHistory {
		b.WriteString("1 9" + strconv.Itoa(i) + "\n")
	}
	must(t, os.MkdirAll(filepath.Dir(path), 0o700))
	must(t, os.WriteFile(path, []byte(b.String()), 0o600))
	g := &guard{}
	got, err := g.loadHistory([]id{{dev: 2, ino: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != maxHistory || got[len(got)-1] != (id{2, 2}) || got[0] == (id{1, 90}) {
		t.Errorf("capped history: %d entries, last %v, first %v", len(got), got[len(got)-1], got[0])
	}
	saved, err := readHistory(path)
	if err != nil || len(saved) != maxHistory {
		t.Errorf("saved history: %d entries, %v", len(saved), err)
	}
}
