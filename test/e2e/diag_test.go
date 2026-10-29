//go:build unix

package e2e

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDiagSessionCanary (T6753): a supervised session whose user types a
// canary and a secret at the composer, whose CLI prints them on stderr,
// and whose hooks carry them in their payloads (the on-screen turn end's
// error, a nested session's id), leaves none of them in diag.db, the
// export, or the bundle. The session's records are there, all in the
// launch's trace, including those of aggy hook and of an agent's refused
// aggy switch, which run as other processes and take the trace from
// AGGY_TRACE.
func TestDiagSessionCanary(t *testing.T) {
	const secret = "ghp_Wm4Kq9Zt2Lx7Bn3Rv8Hc1Jd6Fs5Pa0Ye2Gu"
	const typed, said, printed = "typed-tapir-canary-61", "payload-quokka-canary-62", "stderr-egret-canary-63"
	// G67-7: an env value an agent sets, a model typed in the picker,
	// and a hook event name aggy does not know.
	const envSock, envHome, model, event = "/tmp/env-ocelot-canary-64.sock", "/nonexistent/env-lemur-canary-65", "model-ibex-canary-66", "EventNewtCanary67"
	w := newWorld(t)
	env := []string{"HOME=" + w.home, "AGGY_HOME=" + filepath.Join(w.home, ".aggy"), "PATH=" + w.bin + ":/usr/bin:/bin"}
	aggy := func(args ...string) string {
		t.Helper()
		c := exec.Command(buildAggy(t), args...)
		c.Dir, c.Env = w.cwd, env
		out, err := c.Output()
		if err != nil {
			t.Fatalf("aggy %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	// count is how many records of event (and code, and holding attrs
	// text) the log has: the test waits on these, not on sleeps (G67-R7).
	count := func(event, code, attrs string) int {
		t.Helper()
		n := 0
		for _, l := range strings.Split(aggy("diag", "export", "--jsonl"), "\n") {
			var r struct {
				Event string          `json:"event"`
				Code  string          `json:"code"`
				Attrs json.RawMessage `json:"attrs"`
			}
			if json.Unmarshal([]byte(l), &r) == nil && r.Event == event && r.Code == code && strings.Contains(string(r.Attrs), attrs) {
				n++
			}
		}
		return n
	}
	waitCount := func(what string, n int, event, code, attrs string) {
		t.Helper()
		for end := time.Now().Add(offerTimeout); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
			if count(event, code, attrs) >= n {
				return
			}
		}
		t.Fatalf("no %s record in time", what)
	}
	aggy("setup", "--yes")
	hook := aggySh(t) + " hook --vendor claude \"$1\"; " + aggySh(t) + " switch --new grok >/dev/null 2>&1; true"
	sc := map[string]any{
		"stdout":       "READY>",
		"stderr":       "error: " + printed + " " + secret + "\n",
		"hook_command": []string{"/bin/sh", "-c", hook, "sh"},
		"hooks": []map[string]any{
			{"event": "Stop", "payload": map[string]any{"session_id": said}},
			{"event": "Stop", "payload": map[string]any{"session_id": "{session_id}", "error": said + " " + secret}},
			{"event": event, "payload": map[string]any{"session_id": "{session_id}", "message": said}},
		},
	}
	s := w.start(t, sc, "run", "claude")
	s.waitFor([]byte("READY>"))
	// Every hook has run: the unknown event's hold, and each hook's aggy
	// switch.
	waitCount("holding hook", 1, "hook.event.accepted", "", `"holds":true`)
	waitCount("agent switch", 3, "switch.request.refused", "switch.agent_options", "")
	s.send([]byte{0x1c})
	s.waitFor([]byte("aggy: switch vendor"))
	s.mu.Lock()
	key := []byte("2")
	if strings.Contains(s.out.String(), "1  grok") {
		key = []byte("1")
	}
	s.mu.Unlock()
	s.send([]byte("m" + model + "\r"))
	s.waitFor([]byte(model))
	s.send(key)
	waitCount("queued switch", 1, "switch.request.queued", "", "")
	// The unknown event held the switch; typing ends the hold, and
	// picking grok again (the switch key, then the key the list takes)
	// says no turn is running. The unsent text still blocks it.
	s.send([]byte(typed + " " + secret))
	s.send(append([]byte{0x1c}, key...))
	waitCount("second pick", 2, "switch.picker.chosen", "", "")
	s.waitWithin([]byte("you have not sent"), offerTimeout)
	s.send([]byte{0x1b})
	time.Sleep(300 * time.Millisecond) // past the Esc's wait; the exit is not timed
	s.send([]byte("\r/exit\r"))
	if code := s.exit(); code != 0 {
		t.Fatalf("aggy exit = %d", code)
	}

	// An agent's aggy switch with AGGY_SOCKET or AGGY_HOME set to its own
	// values, in the session's trace (G67-7, G67-R5).
	var launchTrace string
	for _, l := range strings.Split(aggy("diag", "export", "--jsonl", "--event", "supervisor.child.launched"), "\n") {
		var r struct {
			TraceID string `json:"trace_id"`
		}
		if json.Unmarshal([]byte(l), &r) == nil && r.TraceID != "" {
			launchTrace = r.TraceID
		}
	}
	for _, kv := range []string{"AGGY_SOCKET=" + envSock, "AGGY_HOME=" + envHome} {
		c := exec.Command(buildAggy(t), "switch", "grok")
		c.Dir = w.cwd
		c.Env = append(append([]string{}, env...), "AGGY_TRACE="+launchTrace, kv)
		if out, err := c.CombinedOutput(); err == nil {
			t.Errorf("aggy switch with %s: %s", kv, out)
		}
	}

	all := aggy("diag", "sql", "SELECT * FROM records")
	exp := aggy("diag", "export", "--jsonl")
	bundle := aggy("diag", "bundle", "--since", "1h")
	events := aggy("diag", "sql", "SELECT * FROM idx.events")
	log := aggy("log", "-n", "1000")
	for name, text := range map[string]string{"diag.db": all, "export": exp, "bundle": bundle, "idx.events": events, "aggy log": log} {
		for _, leak := range []string{secret, secret[:16], typed, said, printed, envSock, "env-ocelot", envHome, model, event} {
			if strings.Contains(text, leak) {
				t.Errorf("%s holds %q", name, leak)
			}
		}
	}
	type rec struct {
		Event, Code, RunID, TraceID string
	}
	var recs []rec
	for _, l := range strings.Split(strings.TrimSpace(exp), "\n") {
		var r struct {
			Event   string `json:"event"`
			Code    string `json:"code"`
			RunID   string `json:"run_id"`
			TraceID string `json:"trace_id"`
		}
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		recs = append(recs, rec(r))
	}
	var trace, run string
	seen := map[string]string{} // event+code -> run id
	for _, r := range recs {
		if r.Event == "supervisor.child.launched" {
			trace, run = r.TraceID, r.RunID
		}
		seen[r.Event+" "+r.Code] = r.RunID
	}
	if trace == "" {
		t.Fatalf("no launch record in %v", recs)
	}
	for _, r := range recs {
		if r.Event != "setup.vendor.checked" && r.TraceID != trace {
			t.Errorf("%s %s has trace %s, want the session's %s", r.Event, r.Code, r.TraceID, trace)
		}
	}
	for _, want := range []string{
		"hook.event.ignored hook.foreign_session", "hook.event.accepted ", "switch.picker.chosen ",
		"switch.request.queued ", "switch.request.refused switch.unsent_text", "supervisor.child.exited ",
	} {
		if r, ok := seen[want]; !ok || r != run {
			t.Errorf("no %q record from the supervisor's run in %v", want, seen)
		}
	}
	// The agent's aggy switch is its own process, in the session's trace.
	for _, want := range []string{"switch.request.refused switch.agent_options", "switch.request.refused supervisor.unreachable"} {
		if r, ok := seen[want]; !ok || r == run {
			t.Errorf("%s: run %q (supervisor %s), seen %v", want, r, run, seen)
		}
	}
}
