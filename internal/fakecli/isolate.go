package fakecli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/safefs"
)

// IsolatedMain runs a package's tests (call it from TestMain) with HOME,
// AGGY_HOME, and the vendor homes in a temp dir and PATH holding only an
// empty temp dir and /usr/bin:/bin (for git), so a test that reaches
// bare aggy, the index, or a vendor CLI by mistake cannot touch the real
// ones. It fails the run when the real state dir or index changed while
// the tests ran; something else using aggy on the machine at the same
// time would also trip it.
func IsolatedMain(m *testing.M) int {
	real := os.Getenv("AGGY_HOME")
	if real == "" {
		if h, err := os.UserHomeDir(); err == nil {
			real = filepath.Join(h, ".aggy")
		}
	}
	before := stamps(real)
	tmp, err := safefs.MkdirTemp("", "aggy-test-home")
	if err != nil {
		fmt.Fprintln(os.Stderr, "fakecli: isolate:", err)
		return 1
	}
	defer func() { _ = safefs.RemoveAll(tmp) }()
	set := map[string]string{
		"HOME": filepath.Join(tmp, "home"), "AGGY_HOME": filepath.Join(tmp, "aggy"),
		"PATH":              filepath.Join(tmp, "bin") + string(filepath.ListSeparator) + "/usr/bin" + string(filepath.ListSeparator) + "/bin",
		"CLAUDE_CONFIG_DIR": "", "CODEX_HOME": "", "GROK_HOME": "", "AGGY_PARENT": "",
		envGoTool: goTool,
	}
	for _, d := range []string{"home", "aggy", "bin"} {
		if err := safefs.MkdirAll(filepath.Join(tmp, d), 0o700); err != nil {
			fmt.Fprintln(os.Stderr, "fakecli: isolate:", err)
			return 1
		}
	}
	for k, v := range set {
		if err := os.Setenv(k, v); err != nil {
			fmt.Fprintln(os.Stderr, "fakecli: isolate:", err)
			return 1
		}
	}
	code := m.Run()
	if after := stamps(real); real != "" && after != before {
		fmt.Fprintf(os.Stderr, "fakecli: the real aggy state dir %s changed during the tests (%s, then %s)\n", real, before, after)
		return 1
	}
	return code
}

// stamps describes dir and its index files by modified time and size;
// a missing file is "-".
func stamps(dir string) string {
	if dir == "" {
		return ""
	}
	out := ""
	for _, p := range []string{dir, filepath.Join(dir, "aggy.db"), filepath.Join(dir, "aggy.db-wal")} {
		fi, err := safefs.Lstat(p)
		switch {
		case err == nil:
			out += fmt.Sprintf("%s:%d ", fi.ModTime().Format(time.RFC3339Nano), fi.Size())
		case errors.Is(err, fs.ErrNotExist):
			out += "- "
		default:
			out += "? "
		}
	}
	return out
}
