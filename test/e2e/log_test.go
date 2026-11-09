//go:build unix

package e2e

import (
	"strings"
	"testing"
)

// wantLogged (T6925) runs aggy log and fails unless it prints a row of
// event typ whose detail holds detail: a switch the fakes completed, or the
// CLI that came on screen.
func wantLogged(t *testing.T, w *world, typ, detail string) {
	t.Helper()
	code, out := w.aggyCode(t, "log")
	if code != 0 {
		t.Fatalf("aggy log: exit %d\n%s", code, out)
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) > 4 && f[3] == typ && strings.Contains(l, detail) {
			return
		}
	}
	t.Errorf("aggy log has no %s row with %q:\n%s", typ, detail, out)
}
