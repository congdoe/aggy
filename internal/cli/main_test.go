package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/congdoe/aggy/internal/fakecli"
)

// TestMain keeps every test off the real home, index, and vendor CLIs,
// and off the machine's managed-mcp.json (L78-10): it points
// claudeManagedMCP at a file that is not there, and a test that wants
// one points it at its own.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "aggy-managed-")
	if err != nil {
		os.Exit(1)
	}
	claudeManagedMCP = []string{filepath.Join(dir, "managed-mcp.json")}
	code := fakecli.IsolatedMain(m)
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
