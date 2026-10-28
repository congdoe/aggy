//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTrustedPath(t *testing.T) {
	if err := trustedPath("/bin/sh"); err != nil {
		t.Errorf("/bin/sh: %v", err)
	}
	d := t.TempDir()
	open := filepath.Join(d, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(open, "aggy")
	if err := os.WriteFile(bin, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := trustedPath(bin); err == nil || !strings.Contains(err.Error(), "writable by others") {
		t.Errorf("world-writable dir: %v", err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bin, 0o775); err != nil {
		t.Fatal(err)
	}
	if err := trustedPath(bin); err == nil {
		t.Error("group-writable binary passed")
	}
	// A link from a safe place into a writable one is judged by its target.
	if err := os.Chmod(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(d, "aggy")
	if err := os.Symlink(bin, link); err != nil {
		t.Fatal(err)
	}
	if err := trustedPath(link); err == nil {
		t.Error("a link into a writable dir passed")
	}
	_ = os.Chmod(open, 0o755)
}
