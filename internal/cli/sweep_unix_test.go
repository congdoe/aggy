//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/supervisor"
)

// TestSweepMemoryDirs: a memory folder a killed session left behind goes
// at the next run; a live session's, a fresh one, and anything else stay.
func TestSweepMemoryDirs(t *testing.T) {
	isolate(t)
	// A short dir: a Unix socket path must fit in about 100 bytes.
	tmp, err := os.MkdirTemp("/tmp", "aggy-sw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	tmp, _ = filepath.EvalSymlinks(tmp)
	run := filepath.Join(tmp, "run")
	if err := supervisor.PrivateDir(run); err != nil {
		t.Fatal(err)
	}
	const dead, live, fresh = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", "33333333-3333-4333-8333-333333333333"
	old := time.Now().Add(-time.Hour)
	mk := func(name string, age time.Time) string {
		d := filepath.Join(run, name)
		writeAt(t, filepath.Join(d, "memory.md"), "m", 0o600)
		if err := os.Chtimes(d, age, age); err != nil {
			t.Fatal(err)
		}
		return d
	}
	deadDir, liveDir, freshDir, otherDir := mk(dead+".memory", old), mk(live+".memory", old), mk(fresh+".memory", time.Now()), mk("notes.memory", old)
	// Per-launch folders (D19-8) go by their parent.
	deadLaunch, liveLaunch, badNonce := mk(dead+".0a1b2c3d.memory", old), mk(live+".0a1b2c3d.memory", old), mk(dead+".x.memory", old)
	link := filepath.Join(run, "44444444-4444-4444-8444-444444444444.memory")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	// A stale lock file nobody holds is not live.
	writeAt(t, filepath.Join(run, dead+".lock"), "", 0o600)
	var srv *supervisor.Server
	srv, err = supervisor.Listen(run, live, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	sweepMemoryDirs(run, time.Now())
	for _, d := range []string{deadDir, deadLaunch} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("the dead session's folder %s is still there: %v", d, err)
		}
	}
	for _, d := range []string{liveDir, freshDir, otherDir, link, liveLaunch, badNonce} {
		if _, err := os.Lstat(d); err != nil {
			t.Errorf("%s was removed: %v", d, err)
		}
	}
}

// TestMemoryDirPerLaunch (D19-8): two launches of one parent get their
// own folders, both swept by the parent's id.
func TestMemoryDirPerLaunch(t *testing.T) {
	const p = "11111111-1111-4111-8111-111111111111"
	a, err := memoryDir("/r", p)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := memoryDir("/r", p)
	if a == b {
		t.Fatalf("two launches share %s", a)
	}
	for _, d := range []string{a, b} {
		if id, ok := memoryParent(filepath.Base(d)); !ok || id != p {
			t.Errorf("memoryParent(%s) = %q, %v", d, id, ok)
		}
	}
	for _, bad := range []string{"notes.memory", p + ".ABCDEF12.memory", p + ".1234.memory", p + ".lock", p + ".0a1b2c3d.x.memory"} {
		if _, ok := memoryParent(bad); ok {
			t.Errorf("memoryParent(%s) matched", bad)
		}
	}
}
