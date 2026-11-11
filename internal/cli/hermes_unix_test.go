//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/congdoe/aggy/internal/diag"
	"github.com/congdoe/aggy/internal/parity"
	"github.com/congdoe/aggy/internal/policyhook"
	"github.com/congdoe/aggy/internal/store"
	"github.com/congdoe/aggy/internal/supervisor"
)

// shortAggyHome moves AGGY_HOME to a short folder: a socket's path must fit
// in 103 bytes.
func shortAggyHome(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "aggyhm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	d, _ = filepath.EvalSymlinks(d)
	t.Setenv("AGGY_HOME", d)
	return d
}

// TestHermesHook (T6933, #134 review): the hook finds the launch's row in the
// index by profile and session id, then asks that launch's supervisor, with
// the row's token, whether the session is its own and for the decision
// plan's pin; it answers pre_tool_call from that plan, and blocks a call it
// cannot place. It never needs AGGY_* variables: the daemon passes none.
func TestHermesHook(t *testing.T) {
	repo, dir := openCodeWorld(t)
	homeDir := shortAggyHome(t)
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	const profile = "aggy-0123456789ab"
	const sid, other = "local-0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "20261110_101600_d4e5f6"
	d := policyhook.NewDecidePlan("hermes", "0.23.1", false, parity.PolicyPlan{OnScreen: true}, parity.PreApproved{})
	d.Root = repo
	planPath, planSum, err := writeDecidePlan(dir, d)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(homeDir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The launch's supervisor: its socket, its token, and its pin.
	srv, err := supervisor.Listen(runDir, "11111111-1111-4111-8111-111111111111", nil, func(supervisor.Request) {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	srv.SetRoot(1 << 30) // no CLI: no lineage to wait rootWait for; the hook comes from the daemon
	srv.SetToken("tok")
	claim := supervisor.NewHermesClaim(supervisor.HermesPin{Plan: planPath, PlanSum: planSum})
	srv.SetHermesPin(claim.Answer)

	run := func(event string, payload map[string]any) (string, int) {
		t.Helper()
		b, _ := json.Marshal(payload)
		var stdout, stderr bytes.Buffer
		code := hookWith([]string{"hermes", "--home", homeDir, "--profile", profile, "--event", event}, bytes.NewReader(b), os.Getenv, &stdout, &stderr)
		return stdout.String(), code
	}
	call := func(id, cmd string) map[string]any {
		return map[string]any{"hook_event_name": "pre_tool_call", "tool_name": "terminal", "tool_input": map[string]any{"command": cmd},
			"session_id": id, "cwd": "/opt/hermes", "profile": profile, "extra": map[string]any{}}
	}
	blocked := func(name, out string) {
		t.Helper()
		if !strings.Contains(out, `"action":"block"`) {
			t.Fatalf("%s: %q, want a block", name, out)
		}
	}
	// No launch: blocked.
	if out, code := run("pre_tool_call", call(sid, "ls")); code != 0 || !strings.Contains(out, diag.CodeHermesNoLaunch.String()) {
		t.Fatalf("no launch: %q %d", out, code)
	}
	id, err := s.PutHermesLaunch(ctx, store.HermesLaunch{ParentID: "11111111-1111-4111-8111-111111111111", ChildID: "c1", Profile: profile, Root: repo, Cwd: repo, Token: "tok", Started: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	// M1: before the viewer's first turn, a session (a cron job's, a nested
	// hermes') cannot claim the launch.
	out, _ := run("pre_tool_call", call(sid, "ls"))
	blocked("before the first turn", out)
	if ls, _ := s.HermesLaunches(ctx, profile); ls[0].SessionID != "" {
		t.Fatalf("claimed before the first turn: %+v", ls)
	}
	// The person presses Enter: the next session claims it.
	claim.Entered()
	if out, _ := run("pre_tool_call", call(sid, "ls")); out != "" {
		t.Fatalf("ls: %q", out)
	}
	if out, _ := run("pre_tool_call", call(sid, "rm -rf build")); !strings.Contains(out, `"action":"approve"`) {
		t.Fatalf("rm -rf: %q", out)
	}
	if ls, _ := s.HermesLaunches(ctx, profile); len(ls) != 1 || ls[0].SessionID != sid {
		t.Fatalf("launches = %+v", ls)
	}
	// H2: the index names the aggy.db the hook trusts; a command that names it asks.
	if out, _ := run("pre_tool_call", call(sid, `sqlite3 ~/.aggy/aggy.db "update hermes_launches set token=''"`)); !strings.Contains(out, `"action":"approve"`) {
		t.Fatalf("sqlite3 on the index: %q", out)
	}
	// Another profile's payload, on this profile's command: blocked.
	p := call(sid, "ls")
	p["profile"] = "aggy-ba9876543210"
	out, _ = run("pre_tool_call", p)
	blocked("other profile", out)
	// M1: another session in the profile (/new inside Hermes, a nested
	// hermes -p, a delegate) does not move the launch: blocked.
	out, _ = run("pre_tool_call", call(other, "ls"))
	blocked("another session", out)
	// H2: a row rewritten to name that session leads to a supervisor that
	// refuses it.
	if ok, err := s.ClaimHermesSession(ctx, id, sid, other); err != nil || !ok {
		t.Fatalf("rewrite: %v %v", ok, err)
	}
	out, _ = run("pre_tool_call", call(other, "rm -rf build"))
	blocked("rewritten row", out)
	_, _ = s.ClaimHermesSession(ctx, id, other, sid)
	// A bad session id: blocked.
	out, _ = run("pre_tool_call", call("../x", "ls"))
	blocked("bad id", out)
	// A turn end prints nothing.
	end := map[string]any{"hook_event_name": "on_session_end", "session_id": sid, "profile": profile, "extra": map[string]any{"completed": true}}
	if out, code := run("on_session_end", end); out != "" || code != 0 {
		t.Fatalf("turn end: %q %d", out, code)
	}
	// A plan changed under the launch: asked about, never allowed.
	if err := os.WriteFile(planPath, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _ := run("pre_tool_call", call(sid, "ls")); !strings.Contains(out, `"action":"approve"`) {
		t.Fatalf("changed plan: %q", out)
	}
	// H2: a token the row no longer matches, or a supervisor that is gone:
	// asked about, never "no decision".
	srv.SetToken("other")
	if out, _ := run("pre_tool_call", call(sid, "ls")); !strings.Contains(out, `"action":"approve"`) {
		t.Fatalf("wrong token: %q", out)
	}
	_ = srv.Close()
	if out, _ := run("pre_tool_call", call(sid, "ls")); !strings.Contains(out, `"action":"approve"`) {
		t.Fatalf("supervisor gone: %q", out)
	}
	_ = s.DeleteHermesLaunch(ctx, id)
}

// TestHermesHookNoPolicy (#134 review): only the supervisor's own answer of
// --no-policy turns the hook's decisions off.
func TestHermesHookNoPolicy(t *testing.T) {
	repo, _ := openCodeWorld(t)
	homeDir := shortAggyHome(t)
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	const profile = "aggy-0123456789ab"
	const sid = "20261110_101500_a1b2c3"
	runDir := filepath.Join(homeDir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	srv, err := supervisor.Listen(runDir, "99999999-9999-4999-8999-999999999999", nil, func(supervisor.Request) {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	srv.SetRoot(1 << 30) // no CLI: no lineage to wait rootWait for; the hook comes from the daemon
	srv.SetToken("tok")
	srv.SetHermesPin(supervisor.NewHermesClaim(supervisor.HermesPin{NoPolicy: true, Session: sid}).Answer)
	if _, err := s.PutHermesLaunch(ctx, store.HermesLaunch{ParentID: "99999999-9999-4999-8999-999999999999", ChildID: "c9", Profile: profile, Root: repo, Cwd: repo, Token: "tok", SessionID: sid, Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"hook_event_name": "pre_tool_call", "tool_name": "terminal", "tool_input": map[string]any{"command": "rm -rf build"},
		"session_id": sid, "profile": profile})
	var stdout, stderr bytes.Buffer
	hookWith([]string{"hermes", "--home", homeDir, "--profile", profile, "--event", "pre_tool_call"}, bytes.NewReader(b), os.Getenv, &stdout, &stderr)
	if stdout.String() != "" {
		t.Fatalf("no-policy: %q", stdout.String())
	}
}

// TestHermesHookShared (T6954): the hook tells the supervisor of a launch
// with no session yet whether another aggy Hermes launch in the profile is
// live, and blocks the call when that supervisor refuses the claim for it
// (here a --direct launch, which claims only alone).
func TestHermesHookShared(t *testing.T) {
	repo, _ := openCodeWorld(t)
	homeDir := shortAggyHome(t)
	ctx := context.Background()
	s, err := store.OpenDefault(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	const profile = "aggy-0123456789ab"
	const sidA, sidB = "20261110_101500_a1b2c3", "20261110_101600_d4e5f6"
	const idA, idB = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	runDir := filepath.Join(homeDir, "run")
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, l := range []struct {
		id, sid string
		pin     supervisor.HermesPin
	}{
		{idA, sidA, supervisor.HermesPin{NoPolicy: true, Session: sidA}},
		{idB, "", supervisor.HermesPin{NoPolicy: true, Direct: true}},
	} {
		srv, err := supervisor.Listen(runDir, l.id, nil, func(supervisor.Request) {})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = srv.Close() })
		srv.SetRoot(1 << 30)
		srv.SetToken("tok" + l.id)
		srv.SetHermesPin(supervisor.NewHermesClaim(l.pin).Answer)
		if _, err := s.PutHermesLaunch(ctx, store.HermesLaunch{ParentID: l.id, ChildID: "c" + l.id, Profile: profile, Root: repo, Cwd: repo, Token: "tok" + l.id, SessionID: l.sid, Started: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	hookOut := func(id string) string {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"hook_event_name": "pre_tool_call", "tool_name": "terminal", "tool_input": map[string]any{"command": "ls"},
			"session_id": id, "profile": profile})
		var stdout, stderr bytes.Buffer
		hookWith([]string{"hermes", "--home", homeDir, "--profile", profile, "--event", "pre_tool_call"}, bytes.NewReader(b), os.Getenv, &stdout, &stderr)
		return stdout.String()
	}
	// A's own session goes on.
	if out := hookOut(sidA); out != "" {
		t.Fatalf("A's session: %q", out)
	}
	// A new session while A is live (A's /new, or B's first turn): B cannot
	// tell, so the call is blocked, and B stays unclaimed.
	if out := hookOut(sidB); !strings.Contains(out, `"action":"block"`) || !strings.Contains(out, diag.CodeHermesClaimLate.String()) {
		t.Fatalf("shared: %q", out)
	}
	if ls, _ := s.HermesLaunches(ctx, profile); len(ls) != 2 || ls[0].SessionID+ls[1].SessionID != sidA {
		t.Fatalf("launches = %+v", ls)
	}
	// With A gone, B is alone, and its first session claims it.
	ls, _ := s.HermesLaunches(ctx, profile)
	for _, l := range ls {
		if l.ParentID == idA {
			_ = s.DeleteHermesLaunch(ctx, l.ID)
		}
	}
	if out := hookOut(sidB); out != "" {
		t.Fatalf("alone: %q", out)
	}
}
