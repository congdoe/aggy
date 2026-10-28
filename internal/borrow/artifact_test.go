//go:build unix

package borrow

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/adapter"
)

func TestReadArtifact(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	future := time.Now().Add(time.Hour)
	tests := []struct {
		name    string
		kind    string
		setup   func(t *testing.T, p string)
		started time.Time
		want    string // the media type, or a fragment of the refusal
	}{
		{"png", "image", write(pngBytes), time.Time{}, PNG},
		{"jpeg", "image", write(jpgBytes), time.Time{}, JPEG},
		{"mp4", "video", write(mp4Bytes), time.Time{}, MP4},
		{"text as an image", "image", write([]byte("token=abc")), time.Time{}, "not a PNG or JPEG"},
		{"png as a video", "video", write(pngBytes), time.Time{}, "not an MP4"},
		{"made before the child", "image", write(pngBytes), future, "made before the child started"},
		{"symlink", "image", func(t *testing.T, p string) {
			target := filepath.Join(t.TempDir(), "x.png")
			write(pngBytes)(t, target)
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		}, time.Time{}, "not a regular file"},
		{"fifo", "image", func(t *testing.T, p string) {
			if err := syscall.Mkfifo(p, 0o600); err != nil {
				t.Skip(err)
			}
		}, time.Time{}, "not a regular file"},
		{"two links", "image", func(t *testing.T, p string) {
			write(pngBytes)(t, p)
			if err := os.Link(p, p+".2"); err != nil {
				t.Skip(err)
			}
		}, time.Time{}, "more than one hard link"},
		{"over the cap", "image", func(t *testing.T, p string) {
			write(pngBytes)(t, p)
			if err := os.Truncate(p, MaxImageBytes+1); err != nil {
				t.Fatal(err)
			}
		}, time.Time{}, "over the 50 MiB cap"},
		{"missing", "image", func(*testing.T, string) {}, time.Time{}, "no such file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "1.png")
			tt.setup(t, p)
			m, err := readArtifact(adapter.Artifact{Path: p, Kind: tt.kind}, tt.started)
			if strings.Contains(tt.want, "/") {
				if err != nil || m.Type != tt.want {
					t.Fatalf("readArtifact = %q, %v; want %s", m.Type, err, tt.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("readArtifact err = %v, want a refusal with %q", err, tt.want)
			}
		})
	}
}

// TestArtifactRotatedCredential is the attack the gate closes: a child
// hard-links a credential into its image folder and waits for the vendor
// to replace the credential by rename. The old inode then has one link,
// so safefs reads it, but it was made before the child started, and it
// is not an image.
func TestArtifactRotatedCredential(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", "")
	auth := filepath.Join(home, ".codex", "auth.json")
	if err := os.MkdirAll(filepath.Dir(auth), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name    string
		content []byte
		want    string
	}{
		{"a credential", []byte(`{"token":"old"}`), "made before the child started"},
		// Even a credential that happened to start like a PNG is refused
		// for its age.
		{"png-looking credential", pngBytes, "made before the child started"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			write(tt.content)(t, auth)
			link := filepath.Join(t.TempDir(), "1.png")
			if err := os.Link(auth, link); err != nil {
				t.Skip(err)
			}
			// The child starts after the credential was written.
			started := time.Now().Add(timeSlack + time.Second)
			if _, err := readArtifact(adapter.Artifact{Path: link, Kind: "image"}, started); err == nil || !strings.Contains(err.Error(), "credential") && !strings.Contains(err.Error(), "hard link") {
				t.Fatalf("before rotation: err = %v, want a credential or hard-link refusal", err)
			}
			write([]byte(`{"token":"new"}`))(t, auth+".tmp")
			if err := os.Rename(auth+".tmp", auth); err != nil {
				t.Fatal(err)
			}
			if _, err := readArtifact(adapter.Artifact{Path: link, Kind: "image"}, started); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("after rotation: err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	good, bad := filepath.Join(dir, "1.png"), filepath.Join(dir, "2.png")
	write(pngBytes)(t, good)
	write([]byte("not an image"))(t, bad)
	arts := []adapter.Artifact{{Path: good, Kind: "image"}, {Path: bad, Kind: "image"}}
	ok, refused, made := gate(arts, time.Time{}, false)
	if len(ok) != 1 || ok[0].Path != good || ok[0].Ext() != ".png" || len(refused) != 1 || !strings.HasPrefix(refused[0], bad) || made != 2 {
		t.Fatalf("gate = %+v, %q, %d", ok, refused, made)
	}
	// A resumed session's earlier files are skipped and not counted; a
	// fresh session's are refused.
	later := time.Now().Add(time.Hour)
	if ok, refused, made := gate(arts, later, true); len(ok)+len(refused)+made != 0 {
		t.Errorf("resumed gate = %+v, %q, %d", ok, refused, made)
	}
	if ok, refused, made := gate(arts, later, false); len(ok) != 0 || len(refused) != 2 || made != 2 {
		t.Errorf("fresh gate = %+v, %q, %d", ok, refused, made)
	}
}

func write(data []byte) func(t *testing.T, p string) {
	return func(t *testing.T, p string) {
		t.Helper()
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestReadArtifactSniffsFirst (B15-11): a file whose first bytes are not
// the kind's magic is refused without being read whole.
func TestReadArtifactSniffsFirst(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reads := 0
	defer func(f func(io.Reader) ([]byte, error)) { readRest = f }(readRest)
	readRest = func(r io.Reader) ([]byte, error) { reads++; return io.ReadAll(r) }
	p := filepath.Join(t.TempDir(), "1.png")
	write([]byte(strings.Repeat("not an image ", 1000)))(t, p)
	if _, err := readArtifact(adapter.Artifact{Path: p, Kind: "image"}, time.Time{}); err == nil || reads != 0 {
		t.Errorf("err %v, %d whole reads", err, reads)
	}
	write(pngBytes)(t, p)
	if m, err := readArtifact(adapter.Artifact{Path: p, Kind: "image"}, time.Time{}); err != nil || reads != 1 || string(m.Data) != string(pngBytes) {
		t.Errorf("png: %v, %d reads", err, reads)
	}
}
