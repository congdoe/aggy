//go:build unix

package tooldef

import (
	"io/fs"
	"os"
	"syscall"
	"testing"
	"time"
)

type fakeInfo struct{ st *syscall.Stat_t }

func (f fakeInfo) Name() string       { return "demo.toml" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return 0o600 }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return f.st }

// TestNotMine: a file owned by another user, root included, is refused.
// Making one needs root, so the owner check is tested on its input.
func TestNotMine(t *testing.T) {
	me := uint32(os.Getuid())
	for _, c := range []struct {
		uid  uint32
		want bool
	}{{me, false}, {me + 1, true}, {0, me != 0}} {
		var st syscall.Stat_t
		st.Uid = c.uid
		if got := notMine(fakeInfo{&st}) != ""; got != c.want {
			t.Errorf("uid %d: refused %v, want %v", c.uid, got, c.want)
		}
	}
	if notMine(fakeInfo{}) == "" {
		t.Error("an unreadable owner must be refused")
	}
}
