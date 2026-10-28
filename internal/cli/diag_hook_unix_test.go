//go:build unix

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/congdoe/aggy/internal/diag"
)

// TestDiagHookTrace (T6753): aggy hook, run by the on-screen CLI with
// the session's AGGY_TRACE, that cannot reach the supervisor records it
// in the session's trace under its own run id, tags what it prints, and
// stores nothing of the payload: not in diag.db, the export, or the
// bundle. The session itself is test/e2e's TestDiagSessionCanary.
func TestDiagHookTrace(t *testing.T) {
	const secret = "ghp_Tq3Lm8Zx1Rw6Vb4Nk9Hs2Jd7Fc5Pa0Ye8Gu"
	const said = "payload-wombat-canary-52"
	t.Setenv(diag.TraceEnv, "")
	freshRun(t)
	trace := dlog.Trace() // the supervisor's
	t.Setenv(diag.TraceEnv, trace)
	freshRun(t)
	home := os.Getenv("AGGY_HOME")
	env := map[string]string{"AGGY_SOCKET": filepath.Join(home, "run", "0b9d3c4e-1f2a-4b5c-8d7e-6f5a4b3c2d1e.sock"), "AGGY_VENDOR": "claude", diag.TraceEnv: trace}
	tests := []struct {
		payload string
		code    diag.Code
	}{
		{`{"session_id":"` + said + `","error":"` + said + ` ` + secret + `"}`, diag.CodeUnreachable},
		{`["` + said + ` ` + secret + `"]`, diag.CodeBadPayload},
	}
	for _, tt := range tests {
		var out, errb bytes.Buffer
		if code := hookWith([]string{"--vendor", "claude", "Stop"}, strings.NewReader(tt.payload), func(k string) string { return env[k] }, &out, &errb); code != 0 || out.Len() != 0 {
			t.Errorf("aggy hook: code %d stdout %q", code, out.String())
		}
		if !strings.Contains(errb.String(), "["+tt.code.String()+" · run "+dlog.RunID()+"]") {
			t.Errorf("aggy hook's stderr %q has no %s tag", errb.String(), tt.code)
		}
	}
	text := allDiagText(t, home)
	_, exp, _ := runDiagCmd("export", "--jsonl")
	_, bundle, _ := runDiagCmd("bundle", "--since", "1h")
	for name, s := range map[string]string{"diag.db": text, "export": exp, "bundle": bundle} {
		for _, leak := range []string{secret, secret[:16], said} {
			if strings.Contains(s, leak) {
				t.Errorf("%s holds %q", name, leak)
			}
		}
	}
	n := 0
	for _, r := range exportRecords(t, exp) {
		if r.Event == diag.EvHookForwardFailed.String() {
			n++
			if r.TraceID != trace || r.RunID != dlog.RunID() {
				t.Errorf("record trace %s run %s, want trace %s run %s", r.TraceID, r.RunID, trace, dlog.RunID())
			}
		}
	}
	if n != len(tests) {
		t.Errorf("%d hook.forward.failed records, want %d", n, len(tests))
	}
}
