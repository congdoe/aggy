package parity

import (
	"os"
	"testing"

	"github.com/congdoe/aggy/internal/fakecli"
)

// TestMain keeps every test off the real home, index, and vendor CLIs.
func TestMain(m *testing.M) { os.Exit(fakecli.IsolatedMain(m)) }
